package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/safety"
)

func chained(t *testing.T, n int) []string {
	t.Helper()
	var b bytes.Buffer
	c := NewChain(&b, "")
	for i := 0; i < n; i++ {
		// typed values as the executor writes them (a struct map, a []string, large integers)
		c.Record(Event{Time: t0, Type: "applied", RecommendationID: "r", Details: map[string]any{"i": i, "names": []string{"app", "proxy"},
			"previous": map[string]ContainerResources{"app": {CPURequest: "500m", MemRequest: "256Mi"}}, "bytes": int64(268435456)}})
	}
	return strings.Split(strings.TrimSpace(b.String()), "\n")
}

// Editing, deleting, reordering or inserting a line breaks the chain at that line; an intact file verifies.
func TestAuditChainDetectsEditsDeletionsAndInsertions(t *testing.T) {
	lines := chained(t, 4)
	rep, err := VerifyChain(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil || rep.FirstBreak != 0 || rep.Chained != 4 {
		t.Fatalf("%v %+v", err, rep)
	}
	edited := append([]string{}, lines...)
	edited[1] = strings.Replace(edited[1], `"applied"`, `"decision"`, 1)
	deleted := append(append([]string{}, lines[:2]...), lines[3:]...)
	swapped := []string{lines[0], lines[2], lines[1], lines[3]}
	inserted := append([]string{lines[0], lines[1], `{"time":"2026-11-09T12:00:00Z","type":"applied","actor":"x","recommendationId":"r","target":"t"}`}, lines[2:]...)
	for name, c := range map[string]struct {
		in   []string
		line int
	}{"edited": {edited, 2}, "deleted": {deleted, 3}, "swapped": {swapped, 2}, "inserted unchained": {inserted, 3}} {
		rep, _ := VerifyChain(strings.NewReader(strings.Join(c.in, "\n")))
		if rep.FirstBreak != c.line {
			t.Errorf("%s: break at %d, want %d (%s)", name, rep.FirstBreak, c.line, rep.Problem)
		}
	}
	// lines written before chaining existed are allowed at the start
	legacy := append([]string{`{"time":"2026-11-09T12:00:00Z","type":"decision","actor":"x","recommendationId":"old","target":"t"}`}, lines...)
	if rep, _ := VerifyChain(strings.NewReader(strings.Join(legacy, "\n"))); rep.FirstBreak != 0 || rep.Unchained != 1 {
		t.Fatalf("%+v", rep)
	}
}

// A second process appending to the same file continues the chain instead of starting a new one.
func TestAuditChainResumesAcrossProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	for run := 0; run < 2; run++ {
		prev, err := LastHash(p)
		if err != nil {
			t.Fatal(err)
		}
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		c := NewChain(f, prev)
		c.Record(Event{Time: t0, Type: "decision"})
		c.Record(Event{Time: t0, Type: "applied"})
		f.Close()
	}
	f, _ := os.Open(p)
	defer f.Close()
	rep, err := VerifyChain(f)
	if err != nil || rep.FirstBreak != 0 || rep.Chained != 4 {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestHTTPSinkDeliversRetriesAndReportsWhatItCouldNot(t *testing.T) {
	var got, calls atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // first attempt fails, the retry must deliver
			return
		}
		auth.Store(r.Header.Get("Authorization"))
		got.Add(1)
	}))
	defer srv.Close()
	s := NewHTTPSink(srv.URL, "tok", srv.Client())
	s.Record(Event{Time: t0, Type: "applied", Hash: "h"})
	s.Record(Event{Time: t0, Type: "verified"})
	if err := s.Close(); err != nil || got.Load() != 2 || auth.Load() != "Bearer tok" {
		t.Fatalf("%v delivered=%d auth=%v", err, got.Load(), auth.Load())
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer down.Close()
	d := NewHTTPSink(down.URL+"?secret=x", "", down.Client())
	d.Record(Event{Time: t0, Type: "applied"})
	err := d.Close()
	if err == nil || !strings.Contains(err.Error(), "1 audit event") || strings.Contains(err.Error(), "secret=x") {
		t.Fatalf("undelivered events must be reported, without the query string: %v", err)
	}
}

// End to end on the fake cluster: a signed approval lets a policy that requires signatures apply; a forged approved-by stamp
// (anyone with update rights could write it) is refused.
func TestSignedApprovalEndToEnd(t *testing.T) {
	privPEM, pubPEM, id, err := approval.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{5}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := approval.ParsePrivate(privPEM)
	d := dep()
	d.Annotations = nil
	e := newEnv(d)
	e.ex.SignKey = priv
	fakeIdentity(e, "alice")
	if _, err := e.ex.Approve(context.Background(), rec()); err != nil {
		t.Fatal(err)
	}
	p := safety.Policy{AllowNamespaces: []string{"shop"}, RequireSignedApproval: true, ApproverKeys: map[string]string{id: string(pubPEM)}}
	res, err := e.ex.Apply(context.Background(), rec(), p, false)
	if err != nil || !res.Decision.Allowed {
		t.Fatalf("a signed approval must satisfy the policy: %v %+v", err, res.Decision)
	}
	live, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	live.Annotations[AnnApprovedBy] = "mallory"
	if _, err := e.kc.AppsV1().Deployments("shop").Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, _ = e.ex.Apply(context.Background(), rec(), p, false)
	if res.Decision.Allowed || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "signed approval required") {
		t.Fatalf("a forged approver stamp must be refused: %+v", res.Decision)
	}
}

type collect struct{ got []Event }

func (c *collect) Record(e Event) { c.got = append(c.got, e) }

// A copy forwarded after chaining carries the same prev/hash as the file, so the collector's copy verifies on its own.
func TestAuditChainForwardsChainedEvents(t *testing.T) {
	var b bytes.Buffer
	next := &collect{}
	c := NewChain(&b, "")
	c.Next = next
	c.Record(Event{Time: t0, Type: "approved"})
	c.Record(Event{Time: t0, Type: "applied", Details: map[string]any{"previous": map[string]ContainerResources{"app": {CPURequest: "1"}}}})
	var fwd bytes.Buffer
	j := &JSONL{W: &fwd}
	for _, e := range next.got {
		if e.Hash == "" {
			t.Fatal("forwarded event without its hash")
		}
		j.Record(e)
	}
	if fwd.String() != b.String() {
		t.Fatalf("forwarded copy differs from the file:\n%s\n%s", fwd.String(), b.String())
	}
	if rep, _ := VerifyChain(&fwd); rep.FirstBreak != 0 || rep.Chained != 2 {
		t.Fatalf("%+v", rep)
	}
}

// A collector with a private CA is reachable only when that CA is given; verification is never skipped.
func TestAuditHTTPClientTrustsOnlyTheGivenCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, _ := AuditHTTPClient("")
	if _, err := plain.Post(srv.URL, "application/json", strings.NewReader("{}")); err == nil {
		t.Fatal("an unknown CA must be refused")
	}
	hc, err := AuditHTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the given CA must be trusted: %v", err)
	}
	resp.Body.Close()
	bad := filepath.Join(t.TempDir(), "bad.crt")
	_ = os.WriteFile(bad, []byte("not a cert"), 0o600)
	if _, err := AuditHTTPClient(bad); err == nil {
		t.Fatal("a file without a PEM certificate must be refused")
	}
}

// A collector outage shorter than MaxRetry delays events instead of losing them, and keeps their order (docs/LAB.md result 25 found
// that the earlier fixed 4 attempts lost events in a 15 s outage). Past the budget, each loss is reported through OnDrop at once.
func TestHTTPSinkRidesOutAnOutageInOrder(t *testing.T) {
	var up atomic.Bool
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var e Event
		_ = json.NewDecoder(r.Body).Decode(&e)
		mu.Lock()
		order = append(order, e.Type)
		mu.Unlock()
	}))
	defer srv.Close()
	s := NewHTTPSink(srv.URL, "", srv.Client())
	var slept time.Duration
	s.sleep = func(d time.Duration) { // virtual time: the collector comes back after 20 s of backoff
		slept += d
		if slept >= 20*time.Second {
			up.Store(true)
		}
	}
	var drops atomic.Int32
	s.OnDrop = func(string) { drops.Add(1) }
	for _, ty := range []string{"a", "b", "c"} {
		s.Record(Event{Time: t0, Type: ty})
	}
	for s.Delivered() < 3 && drops.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	err := s.Close()
	mu.Lock()
	got := strings.Join(order, "")
	mu.Unlock()
	if err != nil || got != "abc" || drops.Load() != 0 {
		t.Fatalf("err=%v order=%v drops=%d", err, order, drops.Load())
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	d := NewHTTPSink(down.URL, "", down.Client())
	d.sleep = func(time.Duration) {}
	d.MaxRetry = time.Millisecond
	var reasons []string
	d.OnDrop = func(r string) { mu.Lock(); reasons = append(reasons, r); mu.Unlock() }
	d.Record(Event{Time: t0, Type: "x"})
	_ = d.Close() // waits for the sender, so OnDrop has run
	mu.Lock()
	defer mu.Unlock()
	if d.Dropped() != 1 || d.Delivered() != 0 || len(reasons) != 1 || !strings.Contains(reasons[0], "retry budget") {
		t.Fatalf("dropped=%d delivered=%d reasons=%v", d.Dropped(), d.Delivered(), reasons)
	}
}
