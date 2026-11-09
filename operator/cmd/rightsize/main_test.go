package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These test only the argument guards that must hold before any cluster is contacted.

func TestExecuteWithoutExplicitContextIsRefusedBeforeAnythingElse(t *testing.T) {
	dir := t.TempDir()
	rec, pol := filepath.Join(dir, "rec.json"), filepath.Join(dir, "pol.yaml")
	_ = os.WriteFile(rec, []byte("{}"), 0o600)
	_ = os.WriteFile(pol, []byte("{}"), 0o600)
	err := apply([]string{"--recommendation", rec, "--policy", pol, "--execute"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "explicit --context") {
		t.Fatalf("%v", err)
	}
}

func TestApplyRequiresRecommendationAndPolicy(t *testing.T) {
	if err := apply(nil, io.Discard); err == nil {
		t.Fatal("accepted no arguments")
	}
}

func TestPolicyWithUnknownKeysIsRejected(t *testing.T) {
	dir := t.TempDir()
	rec, pol := filepath.Join(dir, "rec.json"), filepath.Join(dir, "pol.yaml")
	_ = os.WriteFile(rec, []byte("{}"), 0o600)
	_ = os.WriteFile(pol, []byte("allowNamespace: [prod]\n"), 0o600) // typo: would silently authorise nothing or the wrong thing
	err := apply([]string{"--recommendation", rec, "--policy", pol, "--context", "x"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("%v", err)
	}
}

func TestRollbackAndReadOnlyCommandsNeedAContext(t *testing.T) {
	if err := rollback([]string{"--namespace", "a", "--workload", "b"}, io.Discard); err == nil {
		t.Error("rollback without --context accepted")
	}
	if err := nodesCmd(nil, io.Discard); err == nil {
		t.Error("nodes without --context accepted")
	}
	if err := costCmd([]string{"--node-hourly-usd", "1"}, io.Discard); err == nil {
		t.Error("cost without --context accepted")
	}
}

func TestCostNeedsExactlyOneBasis(t *testing.T) {
	if err := costCmd([]string{"--context", "x"}, io.Discard); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("%v", err)
	}
	if err := costCmd([]string{"--context", "x", "--billing-file", "b.csv", "--node-hourly-usd", "1"}, io.Discard); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("%v", err)
	}
}

func TestParsePrices(t *testing.T) {
	m, err := parsePrices("a=0.2,b=1")
	if err != nil || m["a"] != 0.2 || m["b"] != 1 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"a", "a=x", "a=-1"} {
		if _, err := parsePrices(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
