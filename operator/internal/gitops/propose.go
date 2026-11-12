// Package gitops turns a recommendation into a reviewable change in a Git repository instead of patching the live object.
//
// For workloads managed by Argo CD or Flux a direct patch is reverted at the next sync. Propose edits the manifest that declares the
// workload (comments and key order are preserved), commits it on a NEW branch and returns to the branch you were on. It never pushes,
// never opens a pull request and never touches a cluster: a person (or CI) reviews the branch and opens the PR.
package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// Options select the repository, the manifest file and the recommendation.
type Options struct {
	RepoDir string
	// File is the manifest path relative to RepoDir; it may hold several YAML documents.
	File string
	// Branch is the new branch name; default rightsize/<kind>-<name>-<id>.
	Branch string
	Rec    recommend.Recommendation
}

// Result describes the proposal.
type Result struct {
	Branch string
	Commit string
	Diff   string
}

// Propose edits the manifest, commits it on a new branch and restores the original branch.
func Propose(o Options) (Result, error) {
	var res Result
	r := o.Rec
	if r.Abstained || !(isChange(r.CPU) || isChange(r.Memory)) {
		return res, errors.New("the recommendation changes nothing (or abstained): there is nothing to propose")
	}
	path := filepath.Join(o.RepoDir, o.File)
	if rel, err := filepath.Rel(o.RepoDir, path); err != nil || strings.HasPrefix(rel, "..") {
		return res, errors.New("the manifest path must stay inside the repository")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	out, err := edit(raw, r)
	if err != nil {
		return res, err
	}

	g := git{dir: o.RepoDir}
	if dirty, err := g.out("status", "--porcelain", "--", o.File); err != nil {
		return res, err
	} else if strings.TrimSpace(dirty) != "" {
		return res, errors.New("the manifest has uncommitted changes: commit or stash them first")
	}
	orig, err := g.out("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return res, err
	}
	orig = strings.TrimSpace(orig)
	branch := o.Branch
	if branch == "" {
		branch = fmt.Sprintf("rightsize/%s-%s-%s", strings.ToLower(r.Kind), r.Workload, r.ID)
	}
	if _, err := g.out("check-ref-format", "--branch", branch); err != nil {
		return res, fmt.Errorf("invalid branch name %q", branch)
	}
	if _, err := g.out("checkout", "-b", branch); err != nil {
		return res, fmt.Errorf("creating branch: %w", err)
	}
	restore := func() { _, _ = g.out("checkout", orig) }
	if err := os.WriteFile(path, out, 0o644); err != nil {
		restore()
		return res, err
	}
	msg := fmt.Sprintf("rightsize: %s/%s %s resources (recommendation %s)\n\ncpu: %.4g -> %.4g cores (%s)\nmemory: %.0f -> %.0f bytes (%s)\nconfidence: %s\nestimated potential saving (not a bill): cpu %.2f + memory %.2f USD/month\n",
		r.Namespace, r.Workload, r.Container, r.ID, r.CPU.Current, r.CPU.Recommended, r.CPU.Action, r.Memory.Current, r.Memory.Recommended, r.Memory.Action,
		r.Confidence, r.CPU.PotentialMonthlyUSD, r.Memory.PotentialMonthlyUSD)
	if _, err := g.out("add", "--", o.File); err != nil {
		restore()
		return res, err
	}
	if _, err := g.out("commit", "-q", "-m", msg); err != nil {
		restore()
		return res, fmt.Errorf("commit failed (is user.name/user.email configured?): %w", err)
	}
	res.Branch = branch
	if c, err := g.out("rev-parse", "HEAD"); err == nil {
		res.Commit = strings.TrimSpace(c)
	}
	if d, err := g.out("diff", orig+".."+branch, "--", o.File); err == nil {
		res.Diff = d
	}
	restore()
	return res, nil
}

func isChange(x recommend.ResourceResult) bool {
	return x.Action == recommend.Increase || x.Action == recommend.Decrease
}

type git struct{ dir string }

func (g git) out(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return so.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(se.String()))
	}
	return so.String(), nil
}

// edit rewrites the matching container in a multi-document manifest.
func edit(raw []byte, r recommend.Recommendation) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []*yaml.Node
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("manifest is not valid YAML: %w", err)
		}
		docs = append(docs, &n)
	}
	found := 0
	for _, d := range docs {
		root := top(d)
		if root == nil || scalar(get(root, "kind")) != r.Kind || scalar(get(get(root, "metadata"), "name")) != r.Workload {
			continue
		}
		if ns := scalar(get(get(root, "metadata"), "namespace")); ns != "" && r.Namespace != "" && ns != r.Namespace {
			continue
		}
		c := container(root, r.Container)
		if c == nil {
			return nil, fmt.Errorf("container %q not found in %s/%s", r.Container, r.Kind, r.Workload)
		}
		if err := setResources(c, r); err != nil {
			return nil, err
		}
		found++
	}
	switch found {
	case 0:
		return nil, fmt.Errorf("no %s named %q in the manifest (Helm templates and Kustomize patches are not edited)", r.Kind, r.Workload)
	case 1:
	default:
		return nil, fmt.Errorf("%d documents match %s/%s: refusing to guess", found, r.Kind, r.Workload)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

func top(d *yaml.Node) *yaml.Node {
	if d.Kind == yaml.DocumentNode && len(d.Content) == 1 && d.Content[0].Kind == yaml.MappingNode {
		return d.Content[0]
	}
	return nil
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func container(root *yaml.Node, name string) *yaml.Node {
	cs := get(get(get(get(root, "spec"), "template"), "spec"), "containers")
	if cs == nil || cs.Kind != yaml.SequenceNode {
		return nil
	}
	for _, c := range cs.Content {
		if scalar(get(c, "name")) == name {
			return c
		}
	}
	return nil
}

func ensureMap(parent *yaml.Node, key string) *yaml.Node {
	if n := get(parent, key); n != nil && n.Kind == yaml.MappingNode {
		return n
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, k, v)
	return v
}

func setScalar(m *yaml.Node, key, val string) {
	if n := get(m, key); n != nil {
		n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, "!!str", val, yaml.DoubleQuotedStyle
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val, Style: yaml.DoubleQuotedStyle})
}

func qty(v string) (float64, bool) {
	if v == "" {
		return 0, true
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, false
	}
	return q.AsApproximateFloat64(), true
}

func setResources(c *yaml.Node, r recommend.Recommendation) error {
	res := ensureMap(c, "resources")
	req := ensureMap(res, "requests")
	lim := ensureMap(res, "limits")
	apply := func(name string, x recommend.ResourceResult, format func(float64) string) error {
		if !isChange(x) {
			return nil
		}
		cur, ok := qty(scalar(get(req, name)))
		if !ok {
			return fmt.Errorf("the %s request in the manifest is not a valid quantity", name)
		}
		if math.Abs(cur-x.Current) > 0.01*math.Max(x.Current, 1e-9) && !(cur == 0 && x.Current == 0) {
			return fmt.Errorf("stale recommendation: the manifest has %s request %.6g but it was computed for %.6g", name, cur, x.Current)
		}
		if x.Limit > 0 {
			l, ok := qty(scalar(get(lim, name)))
			switch {
			case !ok:
				return fmt.Errorf("the %s limit in the manifest is not a valid quantity", name)
			case l == 0:
				return fmt.Errorf("refused: the recommendation would set a new %s limit where none exists", name)
			case x.Limit < l*0.999:
				return fmt.Errorf("refused: the recommendation would lower the %s limit", name)
			}
			setScalar(lim, name, format(x.Limit))
		}
		setScalar(req, name, format(x.Recommended))
		return nil
	}
	cpu := func(v float64) string {
		return resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI).String()
	}
	mem := func(v float64) string { return resource.NewQuantity(int64(math.Round(v)), resource.BinarySI).String() }
	if err := apply("cpu", r.CPU, cpu); err != nil {
		return err
	}
	if err := apply("memory", r.Memory, mem); err != nil {
		return err
	}
	// do not leave empty maps behind
	if len(lim.Content) == 0 {
		res.Content = removeKey(res.Content, "limits")
	}
	return nil
}

func removeKey(c []*yaml.Node, key string) []*yaml.Node {
	for i := 0; i+1 < len(c); i += 2 {
		if c[i].Value == key {
			return append(c[:i:i], c[i+2:]...)
		}
	}
	return c
}
