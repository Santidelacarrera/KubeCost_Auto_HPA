// Package gitops turns a recommendation into a reviewable change in a Git repository instead of patching the live object.
//
// For workloads managed by Argo CD or Flux a direct patch is reverted at the next sync. Propose writes the change where the repository
// declares it (the workload manifest, a Helm values file, or a Kustomize patch; comments and key order are preserved), commits it on a
// NEW branch and returns to the branch you were on. It never touches a cluster. It pushes the branch and opens a pull request only
// when asked (Options.Push, Options.OpenPR); otherwise a person or CI reviews the branch and opens the PR.
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
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// Format is how the recommendation is written into the repository.
type Format string

const (
	// FormatManifest edits the Deployment/StatefulSet/DaemonSet document that declares the workload (the default).
	FormatManifest Format = "manifest"
	// FormatHelmValues edits a Helm values file at the keys named in HelmKeys (one per container).
	FormatHelmValues Format = "helm-values"
	// FormatKustomize writes a strategic-merge patch next to a kustomization.yaml and lists it under `patches`.
	FormatKustomize Format = "kustomize"
)

// Options select the repository, the file and the recommendation.
type Options struct {
	RepoDir string
	// File is relative to RepoDir: the manifest (manifest), the values file (helm-values) or the kustomization.yaml (kustomize).
	File   string
	Format Format
	// HelmKeys maps each container to the dotted key of its resources block in the values file, e.g. app=resources,
	// proxy=sidecar.resources. Required for helm-values; every container the record changes must be mapped.
	HelmKeys map[string]string
	// BaseFile (kustomize only, optional) is the manifest the overlay patches, relative to RepoDir; when set, the current values
	// are checked against it so a stale recommendation is refused.
	BaseFile string
	// Branch is the new branch name; default rightsize/<kind>-<name>-<id>.
	Branch string
	Rec    recommend.Recommendation
	// Push pushes the new branch to Remote (default origin). OpenPR additionally opens a pull request against Base with
	// PROpener (default: the GitHub CLI, `gh pr create`). Both are off by default: nothing leaves the machine unless asked.
	Push     bool
	Remote   string
	OpenPR   bool
	Base     string
	PROpener func(dir, branch, base, title, body string) (string, error)
}

// Result describes the proposal.
type Result struct {
	Branch string
	Commit string
	Diff   string
	Files  []string
	Pushed bool
	PRURL  string
	Notes  []string
}

// Propose writes the change, commits it on a new branch and restores the original branch; with Push/OpenPR it then pushes the
// branch and opens a pull request. The working tree is left as it was.
func Propose(o Options) (Result, error) {
	var res Result
	r := o.Rec
	changes := false
	for _, v := range r.Views() {
		changes = changes || isChange(v.CPU) || isChange(v.Memory)
	}
	if r.Abstained || !changes {
		return res, errors.New("the recommendation changes nothing (or abstained): there is nothing to propose")
	}
	if o.OpenPR && !o.Push {
		return res, errors.New("opening a pull request needs the branch pushed (Push)")
	}
	inside := func(rel string) (string, error) {
		path := filepath.Join(o.RepoDir, rel)
		if rr, err := filepath.Rel(o.RepoDir, path); err != nil || strings.HasPrefix(rr, "..") || rel == "" {
			return "", errors.New("the file path must stay inside the repository")
		}
		return path, nil
	}
	read := func(rel string) ([]byte, error) {
		path, err := inside(rel)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	}
	edits := map[string][]byte{} // repo-relative path -> new content
	switch o.Format {
	case "", FormatManifest:
		raw, err := read(o.File)
		if err != nil {
			return res, err
		}
		out, err := edit(raw, r)
		if err != nil {
			return res, err
		}
		edits[o.File] = out
	case FormatHelmValues:
		raw, err := read(o.File)
		if err != nil {
			return res, err
		}
		out, err := editValues(raw, r, o.HelmKeys)
		if err != nil {
			return res, err
		}
		edits[o.File] = out
	case FormatKustomize:
		if b := filepath.Base(o.File); b != "kustomization.yaml" && b != "kustomization.yml" && b != "Kustomization" {
			return res, errors.New("with the kustomize format the file must be the kustomization.yaml")
		}
		kraw, err := read(o.File)
		if err != nil {
			return res, err
		}
		var base []byte
		if o.BaseFile != "" {
			if base, err = read(o.BaseFile); err != nil {
				return res, err
			}
		} else {
			res.Notes = append(res.Notes, "no base manifest given: the current values were not checked against the repository (pass the base file to refuse stale recommendations)")
		}
		patchRel := filepath.ToSlash(filepath.Join(filepath.Dir(o.File), patchName(r)))
		prior, _ := read(patchRel) // our own earlier patch, if any
		patch, kout, err := kustomizePatch(r, base, prior, kraw, patchName(r))
		if err != nil {
			return res, err
		}
		edits[patchRel], edits[o.File] = patch, kout
	default:
		return res, fmt.Errorf("unknown format %q (manifest, helm-values, kustomize)", o.Format)
	}
	files := make([]string, 0, len(edits))
	for f := range edits {
		files = append(files, f)
	}
	sort.Strings(files)
	res.Files = files

	g := git{dir: o.RepoDir}
	if dirty, err := g.out(append([]string{"status", "--porcelain", "--"}, files...)...); err != nil {
		return res, err
	} else if strings.TrimSpace(dirty) != "" {
		return res, errors.New("a file to change has uncommitted changes: commit or stash them first")
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
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(o.RepoDir, f), edits[f], 0o644); err != nil {
			restore()
			return res, err
		}
	}
	title, body := message(r)
	if _, err := g.out(append([]string{"add", "--"}, files...)...); err != nil {
		restore()
		return res, err
	}
	if _, err := g.out("commit", "-q", "-m", title+"\n\n"+body); err != nil {
		restore()
		return res, fmt.Errorf("commit failed (is user.name/user.email configured?): %w", err)
	}
	res.Branch = branch
	if c, err := g.out("rev-parse", "HEAD"); err == nil {
		res.Commit = strings.TrimSpace(c)
	}
	if d, err := g.out(append([]string{"diff", orig + ".." + branch, "--"}, files...)...); err == nil {
		res.Diff = d
	}
	restore()
	if o.Push {
		remote := o.Remote
		if remote == "" {
			remote = "origin"
		}
		if _, err := g.out("push", "-u", remote, branch); err != nil {
			return res, fmt.Errorf("the branch was committed locally but the push failed: %w", err)
		}
		res.Pushed = true
	}
	if o.OpenPR {
		open := o.PROpener
		if open == nil {
			open = ghPR
		}
		url, err := open(o.RepoDir, branch, o.Base, title, body)
		if err != nil {
			return res, fmt.Errorf("the branch was pushed but opening the pull request failed: %w", err)
		}
		res.PRURL = url
	}
	return res, nil
}

// message is the commit message and the pull request text: what changes, by how much, how sure, and what it is not.
func message(r recommend.Recommendation) (string, string) {
	var b strings.Builder
	views := r.Views()
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Container)
		fmt.Fprintf(&b, "container %s:\n  cpu: %.4g -> %.4g cores (%s)\n  memory: %.0f -> %.0f bytes (%s)\n", v.Container,
			v.CPU.Current, v.CPU.Recommended, v.CPU.Action, v.Memory.Current, v.Memory.Recommended, v.Memory.Action)
	}
	cpu, mem := r.PotentialMonthlyUSD()
	fmt.Fprintf(&b, "confidence: %s\nestimated potential saving (not a bill): cpu %.2f + memory %.2f USD/month\n", r.Confidence, cpu, mem)
	if len(r.Risks) > 0 {
		fmt.Fprintf(&b, "risks:\n  - %s\n", strings.Join(r.Risks, "\n  - "))
	}
	b.WriteString("\nProposed by rightsize from observed usage. Review it like any other change: nothing was applied to a cluster.\n")
	return fmt.Sprintf("rightsize: %s/%s %s resources (recommendation %s)", r.Namespace, r.Workload, strings.Join(names, ","), r.ID), b.String()
}

// ghPR opens the pull request with the GitHub CLI, which uses the person's own `gh auth` session.
func ghPR(dir, branch, base, title, body string) (string, error) {
	args := []string{"pr", "create", "--head", branch, "--title", title, "--body", body}
	if base != "" {
		args = append(args, "--base", base)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr create: %v: %s", err, strings.TrimSpace(se.String()))
	}
	return strings.TrimSpace(so.String()), nil
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
	ed := &edits{}
	for _, d := range docs {
		root := top(d)
		if root == nil || scalar(get(root, "kind")) != r.Kind || scalar(get(get(root, "metadata"), "name")) != r.Workload {
			continue
		}
		if ns := scalar(get(get(root, "metadata"), "namespace")); ns != "" && r.Namespace != "" && ns != r.Namespace {
			continue
		}
		for _, v := range r.Views() {
			c := container(root, v.Container)
			if c == nil {
				return nil, fmt.Errorf("container %q not found in %s/%s", v.Container, r.Kind, r.Workload)
			}
			if err := setResources(c, v, ed); err != nil {
				if v.Container != r.Container {
					return nil, fmt.Errorf("container %s: %w", v.Container, err)
				}
				return nil, err
			}
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
	return render(raw, docs, ed)
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

// setScalar sets m[key] = val. ed (may be nil) records how to make the same change in the source text: an existing scalar is
// replaced in place; adding a key is structural, and the file is then re-encoded.
func setScalar(m *yaml.Node, key, val string, ed *edits) {
	if n := get(m, key); n != nil {
		if ed != nil {
			if n.Kind == yaml.ScalarNode && n.Line > 0 {
				ed.list = append(ed.list, textEdit{line: n.Line, col: n.Column, style: n.Style, old: n.Value, new: val})
			} else {
				ed.structural = true
			}
		}
		n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, "!!str", val, yaml.DoubleQuotedStyle
		return
	}
	if ed != nil {
		ed.structural = true
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val, Style: yaml.DoubleQuotedStyle})
}

// textEdit replaces one scalar's text in the source, so everything else in the file stays byte-for-byte as its author wrote it
// (re-encoding the whole file re-indents every list and touches documents the change has nothing to do with: found by the first
// real `propose --open-pr`, docs/LAB.md result 21).
type textEdit struct {
	line, col int
	style     yaml.Style
	old, new  string
}

type edits struct {
	list       []textEdit
	structural bool
}

func quoteAs(style yaml.Style, v string) (string, bool) {
	switch style {
	case yaml.DoubleQuotedStyle:
		return `"` + v + `"`, !strings.ContainsAny(v, `"\`)
	case yaml.SingleQuotedStyle:
		return "'" + v + "'", !strings.Contains(v, "'")
	case 0:
		return v, true
	}
	return "", false
}

// applyEdits makes the recorded scalar replacements in raw. ok is false when a token is not where the parser said (then the
// caller re-encodes instead); the caller also checks that the result parses to the same tree as the edited one.
func applyEdits(raw []byte, list []textEdit) ([]byte, bool) {
	lines := strings.SplitAfter(string(raw), "\n")
	sort.Slice(list, func(i, j int) bool {
		if list[i].line != list[j].line {
			return list[i].line > list[j].line
		}
		return list[i].col > list[j].col
	})
	for _, e := range list {
		if e.line < 1 || e.line > len(lines) {
			return nil, false
		}
		l := []rune(lines[e.line-1])
		at := e.col - 1
		oldTok, ok1 := quoteAs(e.style, e.old)
		newTok, ok2 := quoteAs(e.style, e.new)
		if !ok1 || !ok2 || at < 0 || at+len([]rune(oldTok)) > len(l) || string(l[at:at+len([]rune(oldTok))]) != oldTok {
			return nil, false
		}
		lines[e.line-1] = string(l[:at]) + newTok + string(l[at+len([]rune(oldTok)):])
	}
	return []byte(strings.Join(lines, "")), true
}

// sameTree compares two parsed YAML trees by kind and value (not style), so `cpu: 500m` and `cpu: "500m"` are equal.
func sameTree(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || len(a.Content) != len(b.Content) {
		return false
	}
	if a.Kind == yaml.ScalarNode && a.Value != b.Value {
		return false
	}
	for i := range a.Content {
		if !sameTree(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// render returns the edited documents: the source with only the changed scalars replaced when that is possible and provably
// equivalent, otherwise the documents re-encoded.
func render(raw []byte, docs []*yaml.Node, ed *edits) ([]byte, error) {
	if !ed.structural {
		if out, ok := applyEdits(raw, ed.list); ok {
			if got, err := decodeAll(out); err == nil && len(got) == len(docs) {
				same := true
				for i := range got {
					same = same && sameTree(got[i], docs[i])
				}
				if same {
					return out, nil
				}
			}
		}
	}
	return encodeAll(docs)
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

func setResources(c *yaml.Node, r recommend.Recommendation, ed *edits) error {
	if ed != nil && (get(c, "resources") == nil || get(get(c, "resources"), "requests") == nil) {
		ed.structural = true
	}
	res := ensureMap(c, "resources")
	req := ensureMap(res, "requests")
	hadLimits := get(res, "limits") != nil
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
			setScalar(lim, name, format(x.Limit), ed)
		}
		setScalar(req, name, format(x.Recommended), ed)
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
		if hadLimits && ed != nil {
			ed.structural = true // an existing empty `limits: {}` was dropped
		}
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
