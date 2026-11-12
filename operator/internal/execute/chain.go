package execute

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Chain writes JSONL audit events that are HASH-CHAINED: each event carries the hash of the previous one and its own hash over
// (previous hash + the event without its hash). Deleting, reordering or editing any line breaks every hash after it, which
// VerifyChain reports with the line number. A local file can still be truncated at the end or rewritten whole by someone with
// write access; ship the events to append-only storage (HTTPSink, or a WORM bucket) to make that visible too.
type Chain struct {
	mu   sync.Mutex
	W    io.Writer
	prev string
	// Next, if set, receives each event AFTER it was chained and written (with prev and hash), so a copy shipped to an
	// append-only collector carries the same chain and can be verified there too.
	Next Sink
}

// NewChain continues the chain of an existing file: prev is the hash of its last chained event ("" for a new file).
func NewChain(w io.Writer, prev string) *Chain { return &Chain{W: w, prev: prev} }

// Record implements Sink.
func (c *Chain) Record(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Prev, e.Hash = c.prev, ""
	// Hash the event in the form a reader will decode it into. Details can hold structs and typed maps (the applied event's
	// previous resources): their JSON keys are in declaration order, but after decoding into map[string]any they re-encode
	// sorted, so hashing the original value would never verify. Found on a real cluster (docs/LAB.md result 20).
	b0, err := json.Marshal(e)
	if err != nil {
		return
	}
	var canon Event
	if err := json.Unmarshal(b0, &canon); err != nil {
		return
	}
	e = canon
	h, err := eventHash(e)
	if err != nil {
		return
	}
	e.Hash = h
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if _, err := c.W.Write(append(b, '\n')); err == nil {
		c.prev = h
		if c.Next != nil {
			c.Next.Record(e)
		}
	}
}

func eventHash(e Event) (string, error) {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(append([]byte(e.Prev+"\n"), b...))
	return hex.EncodeToString(s[:]), nil
}

// LastHash returns the hash of the last chained event in an audit file ("" if the file is missing or has none).
func LastHash(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	last := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Hash != "" {
			last = e.Hash
		}
	}
	return last, sc.Err()
}

// ChainReport is the result of VerifyChain.
type ChainReport struct {
	Lines, Chained, Unchained int
	// FirstBreak is the 1-based line of the first event whose hash or link does not verify (0: the chain is intact).
	FirstBreak int
	Problem    string
	LastHash   string
}

// VerifyChain checks every chained event of an audit file. Unchained lines (written before chaining, or by the plain JSONL sink)
// are counted and allowed only BEFORE the first chained event: an unchained line after it is a break.
func VerifyChain(r io.Reader) (ChainReport, error) {
	var rep ChainReport
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	prev, started := "", false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		rep.Lines++
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			if rep.FirstBreak == 0 {
				rep.FirstBreak, rep.Problem = rep.Lines, "not a JSON audit event"
			}
			continue
		}
		if e.Hash == "" {
			rep.Unchained++
			if started && rep.FirstBreak == 0 {
				rep.FirstBreak, rep.Problem = rep.Lines, "an unchained event after the chain started (inserted or written by another writer)"
			}
			continue
		}
		rep.Chained++
		if rep.FirstBreak != 0 {
			continue
		}
		if started && e.Prev != prev {
			rep.FirstBreak, rep.Problem = rep.Lines, "the link to the previous event does not match (a line was removed, reordered or inserted)"
			continue
		}
		if h, err := eventHash(e); err != nil || h != e.Hash {
			rep.FirstBreak, rep.Problem = rep.Lines, "the event's hash does not match its content (the line was edited)"
			continue
		}
		started, prev = true, e.Hash
	}
	rep.LastHash = prev
	return rep, sc.Err()
}

// HTTPSink ships each event as one JSON POST to an append-only collector (a log store, or a service that writes to a WORM bucket).
// Record never blocks the caller: events go to a bounded queue (1024) and one background sender delivers them IN ORDER. A failed
// event is retried with exponential backoff (0.25 s doubling, at most 5 s apart) for up to MaxRetry (default 60 s) before it is
// given up, so a collector outage shorter than that delays events instead of losing them; while it retries, later events wait in
// the queue. After Close, each remaining event is retried for at most 4 s (about 4 attempts), so shutdown is not held up by a collector that is down.
// Events that are not delivered (queue full, retries exhausted) are counted (Dropped), passed to OnDrop when set, and reported by
// Close: never silently lost. Measured: docs/LAB.md result 25.
type HTTPSink struct {
	URL, Token string
	Client     *http.Client
	// MaxRetry bounds how long one event is retried (0: 60 s). OnDrop, if set, is called once per event not delivered, with why.
	// Set both before the first Record.
	MaxRetry time.Duration
	OnDrop   func(reason string)
	q        chan []byte
	wg       sync.WaitGroup
	mu       sync.Mutex
	dropped  int
	sent     int
	closing  bool
	once     sync.Once
	sleep    func(time.Duration) // tests
}

// NewHTTPSink starts the sender. token may be "" (no Authorization header).
func NewHTTPSink(url, token string, hc *http.Client) *HTTPSink {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	s := &HTTPSink{URL: url, Token: token, Client: hc, q: make(chan []byte, 1024), sleep: time.Sleep}
	s.wg.Add(1)
	go s.run()
	return s
}

// Record implements Sink.
func (s *HTTPSink) Record(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	select {
	case s.q <- b:
	default:
		s.drop("queue full (1024 events waiting)")
	}
}

func (s *HTTPSink) drop(reason string) {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
	if s.OnDrop != nil {
		s.OnDrop(reason)
	}
}

// Dropped and Delivered count the events given up and the events the collector accepted so far.
func (s *HTTPSink) Dropped() int   { s.mu.Lock(); defer s.mu.Unlock(); return s.dropped }
func (s *HTTPSink) Delivered() int { s.mu.Lock(); defer s.mu.Unlock(); return s.sent }

func (s *HTTPSink) run() {
	defer s.wg.Done()
	for b := range s.q {
		// read after the receive: fields set between NewHTTPSink and the first Record are then visible (the channel orders them)
		max := s.MaxRetry
		if max <= 0 {
			max = 60 * time.Second
		}
		ok := s.post(b)
		start, wait := time.Now(), 250*time.Millisecond
		for !ok {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			budget := max
			if closing && budget > 4*time.Second {
				budget = 4 * time.Second // after Close: a short budget (about 4 attempts), so shutdown is not held up
			}
			if time.Since(start)+wait > budget {
				break
			}
			s.sleep(wait)
			if wait *= 2; wait > 5*time.Second {
				wait = 5 * time.Second
			}
			ok = s.post(b)
		}
		if ok {
			s.mu.Lock()
			s.sent++
			s.mu.Unlock()
		} else {
			s.drop("not accepted by the collector within the retry budget")
		}
	}
}

func (s *HTTPSink) post(b []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(b))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Close waits for the queue to drain and returns an error naming how many events were not delivered.
func (s *HTTPSink) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		close(s.q)
	})
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped > 0 {
		return fmt.Errorf("%d audit event(s) could not be delivered to %s", s.dropped, redactURL(s.URL))
	}
	return nil
}

func redactURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i] + "?..."
	}
	return u
}

// AuditHTTPClient returns the client HTTPSink uses: the system roots, plus the PEM CA bundle in caFile when given (a collector with
// a private CA, e.g. one inside the cluster). It never disables verification.
func AuditHTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}, nil
}
