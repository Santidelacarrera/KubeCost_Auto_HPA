package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release pipeline injects the version and commit with -ldflags; this proves the wiring
// end to end on a real build (not just that the variables exist).
func TestVersionIsInjectedAtLinkTime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "manager")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version=0.1.0 -X main.commit=0123abcd", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, out)
	}
	for _, want := range []string{"0.1.0", "0123abcd"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("--version output %q lacks %q", out, want)
		}
	}
}

func TestPlainBuildReportsDevAndNeverEmpty(t *testing.T) {
	if version == "" || buildCommit() == "" {
		t.Fatal("version and commit must never be empty")
	}
	if !strings.Contains(versionString(), "kubecost-auto-hpa-operator") {
		t.Fatal(versionString())
	}
}
