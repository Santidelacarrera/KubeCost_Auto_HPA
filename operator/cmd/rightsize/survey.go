package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/capacity"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// surveyTarget is one workload of a namespace with its containers.
type surveyTarget struct {
	Kind, Name string
	Containers []string
}

// surveyTargets lists the Deployments and StatefulSets of a namespace, sorted, each with the containers of its pod template.
// DaemonSets are left out: their rightsizing is not validated on a cluster (docs/STATUS.md). A workload scaled to zero is kept:
// the recommender will abstain for lack of data and say so.
func surveyTargets(ctx context.Context, kc kubernetes.Interface, ns string) ([]surveyTarget, error) {
	var out []surveyTarget
	ds, err := kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range ds.Items {
		t := surveyTarget{Kind: "Deployment", Name: d.Name}
		for _, c := range d.Spec.Template.Spec.Containers {
			t.Containers = append(t.Containers, c.Name)
		}
		out = append(out, t)
	}
	ss, err := kc.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, s := range ss.Items {
		t := surveyTarget{Kind: "StatefulSet", Name: s.Name}
		for _, c := range s.Spec.Template.Spec.Containers {
			t.Containers = append(t.Containers, c.Name)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+"/"+out[i].Name < out[j].Kind+"/"+out[j].Name })
	return out, nil
}

// runSurvey plans every container of every target with planFn (the `plan` command) and returns the records and, for each
// target that could not be planned, why. One failing workload never stops the survey.
func runSurvey(targets []surveyTarget, planFn func(kind, name, container string, out io.Writer) error) (recs []recommend.Recommendation, problems []string) {
	for _, t := range targets {
		for _, c := range t.Containers {
			var buf bytes.Buffer
			if err := planFn(t.Kind, t.Name, c, &buf); err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s container %s: %v", t.Kind, t.Name, c, err))
				continue
			}
			var r recommend.Recommendation
			if err := json.Unmarshal(buf.Bytes(), &r); err != nil || r.ID == "" {
				problems = append(problems, fmt.Sprintf("%s/%s container %s: the plan did not return a recommendation record", t.Kind, t.Name, c))
				continue
			}
			recs = append(recs, r)
		}
	}
	return recs, problems
}

// surveyCmd: one read-only report for a namespace. It runs `plan` for every container of every Deployment and StatefulSet,
// adds the node analysis, and prints the Markdown report of `render`. Every flag it does not know is passed to `plan`
// (window, minimums, prices, Prometheus TLS), so a survey uses exactly the settings a single plan would.
//
//	rightsize survey --context ctx --namespace shop --prometheus-url http://localhost:9090 [--out-dir dir] [plan flags] > report.md
func surveyCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("survey", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // unknown flags belong to plan: collect them below instead of failing here
	var kctx, ns, outDir, title string
	var withNodes bool
	var price float64
	known := map[string]bool{"context": true, "namespace": true, "out-dir": true, "title": true, "nodes": true, "node-hourly-usd": true}
	var mine, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		isBool := name == "nodes"
		if strings.HasPrefix(a, "-") && known[name] {
			mine = append(mine, a)
			if !strings.Contains(a, "=") && !isBool && i+1 < len(args) {
				i++
				mine = append(mine, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	fs.StringVar(&kctx, "context", "", "kubeconfig context (required).")
	fs.StringVar(&ns, "namespace", "", "namespace to survey (required).")
	fs.StringVar(&outDir, "out-dir", "", "also keep each recommendation record as JSON here (they are what `approve` and `apply` take).")
	fs.StringVar(&title, "title", "", "report title.")
	fs.BoolVar(&withNodes, "nodes", true, "include the cluster's node analysis.")
	fs.Float64Var(&price, "node-hourly-usd", 0, "optional node price for the candidate estimate.")
	if err := fs.Parse(mine); err != nil {
		return err
	}
	if kctx == "" || ns == "" {
		return fmt.Errorf("--context and --namespace are required")
	}
	for _, r := range rest {
		n := strings.TrimLeft(strings.SplitN(r, "=", 2)[0], "-")
		if n == "workload" || n == "container" || n == "kind" || n == "publish" {
			return fmt.Errorf("--%s is not a survey flag: a survey plans every workload and publishes nothing", n)
		}
	}
	c := common{kctx: kctx}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	targets, err := surveyTargets(ctx, ex.KC, ns)
	if err != nil {
		return err
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			return err
		}
	}
	recs, problems := runSurvey(targets, func(kind, name, container string, w io.Writer) error {
		var buf bytes.Buffer
		a := append([]string{"--context", kctx, "--namespace", ns, "--kind", kind, "--workload", name, "--container", container}, rest...)
		if err := planCmd(a, &buf); err != nil {
			return err
		}
		if outDir != "" {
			if err := os.WriteFile(filepath.Join(outDir, fmt.Sprintf("%s-%s-%s.json", strings.ToLower(kind), name, container)), buf.Bytes(), 0o600); err != nil {
				return err
			}
		}
		_, err := w.Write(buf.Bytes())
		return err
	})
	if title == "" {
		title = fmt.Sprintf("Namespace %s on %s", ns, kctx)
	}
	in := renderInputs{Title: title, Recs: recs, Problems: problems, Now: time.Now()}
	if len(targets) == 0 {
		in.Problems = append(in.Problems, fmt.Sprintf("namespace %s has no Deployment or StatefulSet", ns))
	}
	if withNodes {
		snap, err := take(ctx, ex.KC, nil, price)
		if err == nil {
			var rep capacity.Report
			if rep, err = capacity.Analyze(snap.nodes, snap.pods, 0.85); err == nil {
				rep.Signals = snap.signals
				in.Nodes = &rep
			}
		}
		if err != nil {
			in.Problems = append(in.Problems, "node analysis: "+err.Error())
		}
	}
	renderMarkdown(out, in)
	return nil
}
