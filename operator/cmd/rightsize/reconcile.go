package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cost"
)

// reconcileCmd compares two provider billing exports (FOCUS CSV), one for the period before an optimisation and one for the
// period after it, and says which level of evidence the saving has. It reads two files and talks to nothing.
func reconcileCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	var before, after, scope string
	var simulated, applied, kept bool
	fs.StringVar(&before, "before", "", "FOCUS CSV export of the billing period BEFORE the change, as the provider delivers it.")
	fs.StringVar(&after, "after", "", "FOCUS CSV export of the billing period AFTER the change.")
	fs.StringVar(&scope, "scope", "", "keep only rows whose ResourceId or ResourceName contains this text (cluster name, resource group, project). Recommended: anything else in the bill moves the result.")
	fs.BoolVar(&simulated, "simulated", false, "the files are not provider exports (hand-made or generated): the result is labelled and can never be 'reconciled'.")
	fs.BoolVar(&applied, "applied", true, "the optimisation was applied.")
	fs.BoolVar(&kept, "kept", true, "the optimisation was kept (verified, not rolled back).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if before == "" || after == "" {
		return fmt.Errorf("--before and --after are required")
	}
	read := func(path, label string) (cost.PeriodBill, error) {
		f, err := os.Open(path)
		if err != nil {
			return cost.PeriodBill{}, err
		}
		defer f.Close()
		p, err := cost.FocusTotals(f, scope)
		if err != nil {
			return p, fmt.Errorf("%s (%s): %w", label, path, err)
		}
		p.Label, p.Simulated = path, simulated
		return p, nil
	}
	b, err := read(before, "before")
	if err != nil {
		return err
	}
	a, err := read(after, "after")
	if err != nil {
		return err
	}
	rec := cost.ReconcileBills(b, a)
	level, missing := cost.Classify(cost.Evidence{Applied: applied, Kept: kept, Bills: &rec})
	return printJSON(out, struct {
		Level           cost.SavingsLevel
		MissingForNext  []string `json:",omitempty"`
		Reconciliation  cost.BillReconciliation
		HowToReadLevels string
	}{level, missing, rec,
		"estimated: requests x unit price, nothing applied | projected: applied and kept, no cost measured | observed: cost measured in two equivalent windows, not an invoice | reconciled: two comparable provider bills show the invoiced amount fell"})
}
