// Command rightsize applies (or rolls back) an authorised rightsizing recommendation to a Deployment.
//
// It is a dry run unless --execute is given, and --execute additionally requires an explicit --context so it can
// never act on whatever cluster happens to be the current kubectl context. Every step goes to the audit log.
//
//	rightsize apply    --recommendation rec.json --policy policy.yaml --context lab [--execute] [--observe-for 5m]
//	rightsize rollback --namespace shop --workload api --context lab
//	rightsize plan     --context lab --prometheus-url ...  (read-only: recommendation + HPA/VPA/exclusion facts from the cluster)
//	rightsize approve  --recommendation rec.json --context lab   (stamps the approval with the identity the API server reports)
//	rightsize propose  --recommendation rec.json --repo . --file k8s/api.yaml   (GitOps: commit on a new local branch, no push)
//	rightsize report   --audit-file audit.jsonl   (acceptance / rollback rate / estimated savings kept)
//	rightsize nodes    --context lab          (read-only: which nodes could be released)
//	rightsize cost     --context lab --billing-file b.csv | --node-hourly-usd 0.2   (read-only allocation report)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend/promsrc"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/safety"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rightsize plan|publish|approve|apply|rollback|propose|report|backtest|nodes|cost|keygen|audit-verify [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "apply":
		err = apply(os.Args[2:], os.Stdout)
	case "rollback":
		err = rollback(os.Args[2:], os.Stdout)
	case "approve":
		err = approveCmd(os.Args[2:], os.Stdout)
	case "plan":
		err = planCmd(os.Args[2:], os.Stdout)
	case "publish":
		err = publishCmd(os.Args[2:], os.Stdout)
	case "report":
		err = reportCmd(os.Args[2:], os.Stdout)
	case "backtest":
		err = backtestCmd(os.Args[2:], os.Stdout)
	case "propose":
		err = proposeCmd(os.Args[2:], os.Stdout)
	case "nodes":
		err = nodesCmd(os.Args[2:], os.Stdout)
	case "cost":
		err = costCmd(os.Args[2:], os.Stdout)
	case "keygen":
		err = keygenCmd(os.Args[2:], os.Stdout)
	case "audit-verify":
		err = auditVerifyCmd(os.Args[2:], os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	kubeconfig, kctx, actor, auditFile, impersonate string
	auditURL, auditTokenFile, signKey               string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig path (default: standard loading rules).")
	fs.StringVar(&c.kctx, "context", "", "kubeconfig context (REQUIRED with --execute and for rollback).")
	fs.StringVar(&c.impersonate, "as", "", "impersonate this user or service account (system:serviceaccount:ns:name): proves a minimal role is enough.")
	fs.StringVar(&c.actor, "actor", "", "who is responsible (default: the current OS user).")
	fs.StringVar(&c.auditFile, "audit-file", "", "also append JSONL audit events to this file, hash-chained (rightsize audit-verify checks it); they are always printed to stderr.")
	fs.StringVar(&c.auditURL, "audit-url", "", "also POST each audit event as JSON to this append-only collector (https).")
	fs.StringVar(&c.auditTokenFile, "audit-token-file", "", "file with a bearer token for --audit-url.")
}

func (c *common) executor() (*execute.Executor, func(), error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeconfig != "" {
		rules.ExplicitPath = c.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: c.kctx}).ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	if c.impersonate != "" {
		cfg.Impersonate.UserName = c.impersonate
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	actor := c.actor
	if actor == "" {
		actor = "unknown"
		if u, err := user.Current(); err == nil {
			actor = u.Username
		}
	}
	sinks := execute.Multi{&execute.JSONL{W: os.Stderr}}
	closer := func() {}
	var closers []func()
	var chain *execute.Chain
	if c.auditFile != "" {
		prev, err := execute.LastHash(c.auditFile)
		if err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(c.auditFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		chain = execute.NewChain(f, prev)
		sinks = append(sinks, chain)
		closers = append(closers, func() { _ = f.Close() })
	}
	if c.auditURL != "" {
		if !strings.HasPrefix(c.auditURL, "https://") && !strings.HasPrefix(c.auditURL, "http://localhost") && !strings.HasPrefix(c.auditURL, "http://127.0.0.1") {
			return nil, nil, fmt.Errorf("--audit-url must be https (or a localhost collector)")
		}
		tok := ""
		if c.auditTokenFile != "" {
			b, err := os.ReadFile(c.auditTokenFile)
			if err != nil {
				return nil, nil, err
			}
			tok = strings.TrimSpace(string(b))
		}
		hs := execute.NewHTTPSink(c.auditURL, tok, nil)
		if chain != nil {
			chain.Next = hs // ship the CHAINED events, so the collector's copy verifies like the file (found preparing LAB result 21)
		} else {
			sinks = append(sinks, hs) // no local file to continue a chain from: shipped unchained
		}
		closers = append([]func(){func() {
			if err := hs.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "WARNING:", err)
			}
		}}, closers...)
	}
	closer = func() {
		for _, f := range closers {
			f()
		}
	}
	ex := &execute.Executor{KC: kc, Dyn: dyn, Audit: sinks, Actor: actor}
	if c.signKey != "" {
		b, err := os.ReadFile(c.signKey)
		if err != nil {
			return nil, nil, err
		}
		if ex.SignKey, err = approval.ParsePrivate(b); err != nil {
			return nil, nil, fmt.Errorf("--sign-key: %w", err)
		}
	}
	return ex, closer, nil
}

func apply(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var recFile, polFile, sloURL, sloQuery string
	var exec bool
	var th execute.Thresholds
	fs.StringVar(&recFile, "recommendation", "", "JSON written by cmd/recommend (required).")
	fs.StringVar(&polFile, "policy", "", "policy YAML/JSON (required; an empty policy authorises nothing).")
	fs.BoolVar(&exec, "execute", false, "really change the workload (default: dry run).")
	fs.DurationVar(&th.ObserveFor, "observe-for", 5*time.Minute, "how long the change must stay healthy.")
	fs.DurationVar(&th.RolloutTimeout, "rollout-timeout", 5*time.Minute, "max time for the rollout to complete.")
	fs.DurationVar(&th.Poll, "poll", 10*time.Second, "observation interval.")
	fs.IntVar(&th.MaxRestarts, "max-restarts", 0, "container restarts tolerated after the change.")
	fs.StringVar(&sloURL, "slo-prometheus-url", "", "Prometheus used for the SLO check during observation.")
	fs.StringVar(&sloQuery, "slo-breach-query", "", "PromQL that is > 0 while the SLO is breached.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if recFile == "" || polFile == "" {
		return fmt.Errorf("--recommendation and --policy are required")
	}
	if exec && c.kctx == "" {
		return fmt.Errorf("--execute requires an explicit --context (never the implicit current context)")
	}
	var rec recommend.Recommendation
	if b, err := os.ReadFile(recFile); err != nil {
		return err
	} else if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("recommendation: %w", err)
	}
	var pol safety.Policy
	if b, err := os.ReadFile(polFile); err != nil {
		return err
	} else if err := yaml.UnmarshalStrict(b, &pol); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	if sloURL != "" && sloQuery != "" {
		hc, err := httpx.NewClient(httpx.Options{}, 30*time.Second)
		if err != nil {
			return err
		}
		pc := &promsrc.Client{BaseURL: sloURL, HTTP: hc}
		th.SLO = func(ctx context.Context) (bool, bool, error) { return pc.Breached(ctx, sloQuery, time.Now()) }
	}
	ctx := context.Background()
	if !exec {
		res, err := ex.Apply(ctx, rec, pol, false)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"mode": "dry-run", "allowed": res.Decision.Allowed, "reasons": res.Decision.Reasons,
			"needsApproval": res.Decision.NeedsApproval, "facts": res.Facts})
	}
	res, v, err := ex.ApplyWatchRollback(ctx, rec, pol, th)
	summary := map[string]any{"mode": "execute", "allowed": res.Decision.Allowed, "reasons": res.Decision.Reasons, "applied": res.Applied,
		"healthy": v.Healthy, "degradedReasons": v.Reasons, "rolledBack": res.Applied && !v.Healthy && err == nil}
	if perr := printJSON(out, summary); perr != nil && err == nil {
		err = perr
	}
	return err
}

func rollback(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var ns, wl, kind string
	var wait time.Duration
	fs.StringVar(&ns, "namespace", "", "namespace (required).")
	fs.StringVar(&wl, "workload", "", "workload name (required).")
	fs.StringVar(&kind, "kind", "Deployment", "Deployment, StatefulSet or DaemonSet.")
	fs.DurationVar(&wait, "wait", 5*time.Minute, "how long to wait for the restored rollout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if ns == "" || wl == "" || c.kctx == "" {
		return fmt.Errorf("--namespace, --workload and --context are required")
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	if err := ex.Rollback(context.Background(), kind, ns, wl, []string{"manual rollback by " + ex.Actor}, wait); err != nil {
		return err
	}
	return printJSON(out, map[string]any{"rolledBack": true, "namespace": ns, "workload": wl})
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
