package main

import (
	"errors"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/license"
)

// licenseState is what the operator knows about its licence after start-up.
type licenseState struct {
	// Effective is the configuration the operator must run with. Without enforcement it equals what was requested.
	Effective license.Effective
	// Tier and Customer describe the licence that verified ("starter", "" when none did).
	Tier     license.Tier
	Customer string
	// Problem is why no licence verified; empty when one did or when none was configured and nothing enforces one.
	Problem  string
	Enforced bool
}

// resolveLicense reads and verifies the licence, and applies it only when enforce is true. It never fails the start-up: a
// licence problem means the starter tier (observe-only), which is the safe state. nodes may return 0 for "unknown".
func resolveLicense(file, pubKeyFile string, enforce bool, req license.Requested, nodes func() int, now time.Time) licenseState {
	st := licenseState{Effective: license.Effective{Tier: license.Starter, ObserveOnly: req.ObserveOnly, Applier: req.Applier}, Tier: license.Starter, Enforced: enforce}
	var lic license.License
	var verr error
	switch {
	case file == "":
		verr = errors.New("no licence file is configured (--license-file)")
	case pubKeyFile == "":
		verr = errors.New("no licence public key is configured (--license-public-key-file)")
	default:
		data, err := os.ReadFile(file)
		if err != nil {
			verr = errors.New("the licence file cannot be read") // the path may be sensitive: keep it out of status and metrics
			break
		}
		pub, err := os.ReadFile(pubKeyFile)
		if err != nil {
			verr = errors.New("the licence public key cannot be read")
			break
		}
		lic, verr = license.Verify(data, []string{string(pub)}, now)
	}
	if verr == nil {
		st.Tier, st.Customer = lic.Tier, lic.Customer
	} else if file != "" || enforce {
		st.Problem = verr.Error()
	}
	if !enforce {
		return st // reported, not applied: the operator behaves exactly as it was configured
	}
	n := 0
	if verr == nil && lic.MaxNodes > 0 {
		n = nodes()
	}
	st.Effective = license.Enforce(req, lic, verr, n)
	st.Tier = st.Effective.Tier
	return st
}

// registerLicenseInfo exports kubecost_license_info{tier,enforced,valid}=1. The customer name is not a label: metrics travel.
func registerLicenseInfo(st licenseState) {
	valid := "true"
	if st.Problem != "" {
		valid = "false"
	}
	enforced := "false"
	if st.Enforced {
		enforced = "true"
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "kubecost_license_info",
		Help:        "Licence the operator runs under; the value is always 1. Without enforcement the tier is informational.",
		ConstLabels: prometheus.Labels{"tier": string(st.Tier), "enforced": enforced, "valid": valid},
	})
	g.Set(1)
	ctrlmetrics.Registry.MustRegister(g)
}
