package httpx

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBearerTokenFileIsReRead(t *testing.T) {
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer s.Close()
	f := filepath.Join(t.TempDir(), "token")
	os.WriteFile(f, []byte("one\n"), 0o600)
	c, err := NewClient(Options{BearerTokenFile: f}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Get(s.URL)
	if got != "Bearer one" {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(f, []byte("two"), 0o600)
	c.Get(s.URL)
	if got != "Bearer two" {
		t.Fatalf("rotation not picked up: %q", got)
	}
}

func TestStaticTokenAndNone(t *testing.T) {
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer s.Close()
	c, _ := NewClient(Options{BearerToken: "abc"}, time.Second)
	c.Get(s.URL)
	if got != "Bearer abc" {
		t.Fatal(got)
	}
	c, _ = NewClient(Options{}, time.Second)
	c.Get(s.URL)
	if got != "" {
		t.Fatalf("no token expected, got %q", got)
	}
}

func TestTLSWithCustomCAAndMTLS(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	s.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	s.StartTLS()
	defer s.Close()
	dir := t.TempDir()
	leaf := s.Certificate()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o600)

	// trusted CA but no client cert: handshake must fail
	c, err := NewClient(Options{CAFile: ca}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(s.URL); err == nil {
		t.Fatal("expected mTLS failure without a client certificate")
	}
	// default trust store (no CA): must reject the self-signed server
	c, _ = NewClient(Options{}, time.Second)
	if _, err := c.Get(s.URL); err == nil {
		t.Fatal("must not trust an unknown CA")
	}
	// with a client cert: succeeds
	certPEM, keyPEM := selfSigned(t)
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, certPEM, 0o600)
	os.WriteFile(kf, keyPEM, 0o600)
	c, err = NewClient(Options{CAFile: ca, CertFile: cf, KeyFile: kf}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(s.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

func TestBadOptions(t *testing.T) {
	if _, err := NewClient(Options{CertFile: "x"}, time.Second); err == nil {
		t.Fatal("cert without key must fail")
	}
	if _, err := NewClient(Options{CAFile: "/nonexistent"}, time.Second); err == nil {
		t.Fatal("missing CA must fail")
	}
	f := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(f, []byte("not pem"), 0o600)
	if _, err := NewClient(Options{CAFile: f}, time.Second); err == nil {
		t.Fatal("invalid CA must fail")
	}
}
