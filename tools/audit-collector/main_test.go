package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestRejectBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name, method, token, body string
		status                    int
	}{
		{"method", "GET", "test-only", "{}", 405},
		{"credentials", "POST", "wrong", "{}", 401},
		{"array", "POST", "test-only", "[]", 400},
		{"multiple", "POST", "test-only", "{} {}", 400},
		{"oversize", "POST", "test-only", strings.Repeat("x", (1<<20)+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sv := &server{token: "test-only"} // no store: rejection must not reach it
			req := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			out := httptest.NewRecorder()
			sv.ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("got %d, want %d", out.Code, tc.status)
			}
		})
	}
}

func testStore(t *testing.T, handler http.HandlerFunc) *store {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	client, err := minio.New(strings.TrimPrefix(backend.URL, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4("local-test", "local-test", ""), Secure: false, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &store{c: client, bucket: "test-audit"}
}

func TestRetentionHeadersAndStorageFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "denied"}[fail], func(t *testing.T) {
			st := testStore(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/test-audit/events/") {
					t.Errorf("unexpected storage request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" {
					t.Error("retention mode missing")
				}
				until, err := time.Parse(time.RFC3339, r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"))
				if err != nil || until.Before(time.Now().Add(23*time.Hour)) {
					t.Error("retention deadline missing or too short")
				}
				io.Copy(io.Discard, r.Body)
				if fail {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
					return
				}
				w.Header().Set("ETag", `"test-etag"`)
			})
			sv := &server{st: st, mode: minio.Compliance, retain: 24 * time.Hour}
			out := httptest.NewRecorder()
			sv.ServeHTTP(out, httptest.NewRequest("POST", "/", strings.NewReader(`{"event":"test"}`)))
			want := http.StatusNoContent
			if fail {
				want = http.StatusServiceUnavailable
			}
			if out.Code != want {
				t.Fatalf("got %d, want %d", out.Code, want)
			}
		})
	}
}

func TestNoAcknowledgementBeforeStorageAndCancelledWaiter(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	st := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"test-etag"`)
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	sv := &server{st: st, mode: minio.Compliance, retain: time.Hour}
	out := httptest.NewRecorder()
	go func() {
		defer close(done)
		sv.ServeHTTP(out, httptest.NewRequest("POST", "/", strings.NewReader("{}")))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("storage not reached")
	}
	select {
	case <-done:
		t.Fatal("acknowledged before storage completed")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiting := httptest.NewRequest("POST", "/", strings.NewReader("{}")).WithContext(ctx)
	waiterDone := make(chan struct{})
	waiterOut := httptest.NewRecorder()
	go func() { defer close(waiterDone); sv.ServeHTTP(waiterOut, waiting) }()
	select {
	case <-waiterDone:
		if waiterOut.Code != http.StatusServiceUnavailable {
			t.Fatalf("cancelled event got %d", waiterOut.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request waited behind the writer")
	}
	close(release)
	released = true
	select {
	case <-done:
		if out.Code != http.StatusNoContent {
			t.Fatalf("stored event got %d", out.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stored event was not acknowledged")
	}
}
