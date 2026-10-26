package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Set at link time (see the Dockerfile and docs/RELEASING.md):
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=<sha>"
//
// A plain `go build` reports "dev"/"unknown" for both; the commit then falls back to the VCS
// revision Go embeds, when there is one.
var (
	version = "dev"
	commit  = "unknown"
)

// buildCommit is the injected commit, or the embedded VCS revision when none was injected.
func buildCommit() string {
	if commit != "unknown" && commit != "" {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}

func versionString() string {
	return fmt.Sprintf("kubecost-auto-hpa-operator %s (commit %s, %s)", version, buildCommit(), runtime.Version())
}

func registerBuildInfo() {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "kubecost_auto_hpa_build_info",
		Help:        "Build information of the running operator; the value is always 1.",
		ConstLabels: prometheus.Labels{"version": version, "commit": buildCommit(), "goversion": runtime.Version()},
	})
	g.Set(1)
	ctrlmetrics.Registry.MustRegister(g)
}
