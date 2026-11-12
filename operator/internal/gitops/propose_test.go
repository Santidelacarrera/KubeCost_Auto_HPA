package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

const manifest = `# platform team: do not remove
apiVersion: v1
kind: Service
metadata:
  name: api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: shop
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: sidecar
        image: busybox
      - name: app            # the main container
        image: example/api:1
        resources:
          requests:
            cpu: "2"
            memory: 4Gi
          limits:
            cpu: "4"
            memory: 4Gi
`

func rec() recommend.Recommendation {
	return recommend.Recommendation{ID: "abc123", Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Confidence: recommend.ConfMed,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: 2, Recommended: 1, Limit: 4},
		Memory: recommend.ResourceResult{Action: recommend.Decrease, Current: 4 << 30, Recommended: 2 << 30, Limit: 4 << 30}}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	run(t, dir, "config", "commit.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(dir, "k8s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "k8s", "api.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestProposeCommitsOnANewBranchAndRestoresTheOriginal(t *testing.T) {
	dir := repo(t, manifest)
	res, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "rightsize/deployment-api-abc123" || res.Commit == "" {
		t.Fatalf("%+v", res)
	}
	if got := run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("the original branch must be restored, on %q", got)
	}
	// the working copy on main is untouched
	b, _ := os.ReadFile(filepath.Join(dir, "k8s", "api.yaml"))
	if strings.ReplaceAll(string(b), "\r\n", "\n") != manifest { // git autocrlf on Windows rewrites line endings
		t.Fatal("the original branch was modified")
	}
	changed := run(t, dir, "show", res.Branch+":k8s/api.yaml")
	for _, want := range []string{`cpu: "1"`, `memory: 2Gi`, "# platform team: do not remove", "# the main container", "kind: Service"} {
		if !strings.Contains(changed, want) {
			t.Errorf("missing %q in:\n%s", want, changed)
		}
	}
	if !strings.Contains(changed, `cpu: "4"`) || !strings.Contains(changed, `memory: 4Gi`) && !strings.Contains(changed, `memory: "4Gi"`) {
		t.Errorf("limits must be unchanged:\n%s", changed)
	}
	msg := run(t, dir, "log", "-1", "--format=%B", res.Branch)
	if !strings.Contains(msg, "abc123") || !strings.Contains(msg, "not a bill") {
		t.Fatal(msg)
	}
	if !strings.Contains(res.Diff, "-") || !strings.Contains(res.Diff, "+") {
		t.Fatal("no diff returned")
	}
}

func TestProposeRefusals(t *testing.T) {
	dir := repo(t, manifest)
	cases := map[string]func(*Options){
		"abstained":    func(o *Options) { o.Rec.Abstained = true },
		"no change":    func(o *Options) { o.Rec.CPU.Action, o.Rec.Memory.Action = recommend.NoChange, recommend.NoChange },
		"stale":        func(o *Options) { o.Rec.CPU.Current = 3 },
		"other name":   func(o *Options) { o.Rec.Workload = "web" },
		"other cont":   func(o *Options) { o.Rec.Container = "nope" },
		"lower limit":  func(o *Options) { o.Rec.CPU.Limit = 2 },
		"escape repo":  func(o *Options) { o.File = "../etc/passwd" },
		"missing file": func(o *Options) { o.File = "k8s/none.yaml" },
		"bad branch":   func(o *Options) { o.Branch = "bad branch name" },
	}
	for name, mut := range cases {
		o := Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec()}
		mut(&o)
		if _, err := Propose(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if got := run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("a refusal left the repository on %q", got)
	}
	if run(t, dir, "branch", "--list", "rightsize/*") != "" {
		t.Fatal("a refused proposal left a branch behind")
	}
}

func TestProposeRefusesDirtyManifestAndAmbiguousMatch(t *testing.T) {
	dir := repo(t, manifest)
	_ = os.WriteFile(filepath.Join(dir, "k8s", "api.yaml"), []byte(manifest+"# local edit\n"), 0o644)
	if _, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec()}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("%v", err)
	}
	twice := manifest + "---\n" + manifest[strings.Index(manifest, "apiVersion: apps/v1"):]
	dir = repo(t, twice)
	if _, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec()}); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("%v", err)
	}
}

func TestProposeCreatesMissingResourcesBlock(t *testing.T) {
	m := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: x\n"
	dir := repo(t, m)
	r := rec()
	r.CPU = recommend.ResourceResult{Action: recommend.Increase, Current: 0, Recommended: 0.5}
	r.Memory = recommend.ResourceResult{Action: recommend.NoChange}
	res, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: r})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "show", res.Branch+":k8s/api.yaml"); !strings.Contains(got, `cpu: "500m"`) || strings.Contains(got, "limits") {
		t.Fatal(got)
	}
}

// Only the changed values change: every other byte of the file, its list indentation and the other documents included, is kept
// (the first real --open-pr re-indented a whole file, docs/LAB.md result 21).
func TestProposeChangesOnlyTheChangedLines(t *testing.T) {
	dir := repo(t, manifest)
	res, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec()})
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Split(strings.TrimRight(manifest, "\n"), "\n") // run() trims the output
	after := strings.Split(strings.ReplaceAll(run(t, dir, "show", res.Branch+":k8s/api.yaml"), "\r\n", "\n"), "\n")
	if len(after) != len(before) {
		t.Fatalf("line count changed: %d -> %d", len(before), len(after))
	}
	var diff []string
	for i := range before {
		if before[i] != after[i] {
			diff = append(diff, before[i]+" => "+after[i])
		}
	}
	if len(diff) != 2 || !strings.Contains(diff[0], `cpu: "2" => `) || !strings.Contains(diff[1], "memory: 4Gi => ") {
		t.Fatalf("only the cpu and memory request lines may change, got %q", diff)
	}
}
