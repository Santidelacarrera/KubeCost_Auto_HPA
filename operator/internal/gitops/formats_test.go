package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "fixture")
}

func withSidecar() recommend.Recommendation {
	r := rec()
	r.Additional = []recommend.ContainerResult{{Container: "sidecar", Confidence: recommend.ConfMed,
		CPU: recommend.ResourceResult{Action: recommend.Decrease, Current: 0.5, Recommended: 0.25}}}
	return r
}

const values = `# chart values: owned by the platform team
replicaCount: 2
resources:
  requests:
    cpu: "2"
    memory: 4Gi
  limits:
    cpu: "4"
    memory: 4Gi
sidecar:
  image: busybox # keep
  resources:
    requests:
      cpu: 500m
`

func TestProposeHelmValuesEditsTheChartKeysAndKeepsComments(t *testing.T) {
	dir := repo(t, manifest)
	write(t, dir, "chart/values.yaml", values)
	commitAll(t, dir)
	res, err := Propose(Options{RepoDir: dir, File: "chart/values.yaml", Format: FormatHelmValues, Rec: withSidecar(),
		HelmKeys: map[string]string{"app": "resources", "sidecar": "sidecar.resources"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(run(t, dir, "show", res.Branch+":chart/values.yaml"), `"`, "") // the file's own quoting is kept
	for _, want := range []string{"# chart values: owned by the platform team", "# keep", `cpu: 1`, `memory: 2Gi`, `cpu: 250m`, `cpu: 4`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(run(t, dir, "log", "-1", "--format=%s", res.Branch), "app,sidecar") {
		t.Fatal("the commit must name both containers")
	}
	for name, keys := range map[string]map[string]string{
		"unmapped container": {"app": "resources"},
		"not a resources":    {"app": "resources", "sidecar": "sidecar.image"},
		"invented layout":    {"app": "resources", "sidecar": "proxy.resources"},
	} {
		if _, err := Propose(Options{RepoDir: dir, File: "chart/values.yaml", Format: FormatHelmValues, Rec: withSidecar(), HelmKeys: keys,
			Branch: "x-" + strings.ReplaceAll(name, " ", "-")}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	stale := withSidecar()
	stale.CPU.Current = 3
	if _, err := Propose(Options{RepoDir: dir, File: "chart/values.yaml", Format: FormatHelmValues, Rec: stale, Branch: "stale",
		HelmKeys: map[string]string{"app": "resources", "sidecar": "sidecar.resources"}}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("a stale recommendation must be refused: %v", err)
	}
}

var manifestWithSidecarRequests = strings.Replace(manifest, "      - name: sidecar\n        image: busybox\n",
	"      - name: sidecar\n        image: busybox\n        resources:\n          requests:\n            cpu: 500m\n", 1)

func TestProposeKustomizeWritesAPatchListsItAndChecksTheBase(t *testing.T) {
	dir := repo(t, manifestWithSidecarRequests)
	write(t, dir, "overlays/prod/kustomization.yaml", "resources:\n- ../../k8s\n# overlay for prod\n")
	commitAll(t, dir)
	res, err := Propose(Options{RepoDir: dir, File: "overlays/prod/kustomization.yaml", Format: FormatKustomize, BaseFile: "k8s/api.yaml", Rec: withSidecar()})
	if err != nil {
		t.Fatal(err)
	}
	patch := run(t, dir, "show", res.Branch+":overlays/prod/rightsize-deployment-api.yaml")
	for _, want := range []string{"kind: Deployment", "name: api", "namespace: shop", "name: app", `cpu: "1"`, "name: sidecar", `cpu: "250m"`} {
		if !strings.Contains(patch, want) {
			t.Errorf("missing %q in the patch:\n%s", want, patch)
		}
	}
	k := run(t, dir, "show", res.Branch+":overlays/prod/kustomization.yaml")
	if !strings.Contains(k, "path: rightsize-deployment-api.yaml") || !strings.Contains(k, "# overlay for prod") {
		t.Fatalf("kustomization:\n%s", k)
	}
	if len(res.Files) != 2 || len(res.Notes) != 0 {
		t.Fatalf("%+v", res)
	}
	stale := withSidecar()
	stale.CPU.Current = 3
	if _, err := Propose(Options{RepoDir: dir, File: "overlays/prod/kustomization.yaml", Format: FormatKustomize, BaseFile: "k8s/api.yaml",
		Rec: stale, Branch: "stale"}); err == nil || !strings.Contains(err.Error(), "base manifest") {
		t.Fatalf("stale against the base must be refused: %v", err)
	}
	if res, err := Propose(Options{RepoDir: dir, File: "overlays/prod/kustomization.yaml", Format: FormatKustomize, Rec: rec(), Branch: "nobase"}); err != nil || len(res.Notes) == 0 {
		t.Fatalf("without a base the result must say the values were not checked: %v %+v", err, res)
	}
	if _, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Format: FormatKustomize, Rec: rec(), Branch: "wrongfile"}); err == nil {
		t.Fatal("the kustomize format needs the kustomization file")
	}
}

// A later recommendation updates OUR earlier patch and is checked against the values in it.
func TestProposeKustomizeUpdatesItsOwnEarlierPatch(t *testing.T) {
	dir := repo(t, manifest)
	write(t, dir, "ov/kustomization.yaml", "resources:\n- ../k8s\npatches:\n- path: rightsize-deployment-api.yaml\n")
	write(t, dir, "ov/rightsize-deployment-api.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        resources:\n          requests:\n            cpu: \"1\"\n")
	commitAll(t, dir)
	next := rec()
	next.ID = "def456"
	next.CPU = recommend.ResourceResult{Action: recommend.Decrease, Current: 1, Recommended: 0.75}
	next.Memory = recommend.ResourceResult{Action: recommend.NoChange}
	res, err := Propose(Options{RepoDir: dir, File: "ov/kustomization.yaml", Format: FormatKustomize, Rec: next})
	if err != nil {
		t.Fatal(err)
	}
	patch := run(t, dir, "show", res.Branch+":ov/rightsize-deployment-api.yaml")
	if !strings.Contains(patch, `cpu: "750m"`) || strings.Count(patch, "name: app") != 1 {
		t.Fatalf("%s", patch)
	}
	if k := run(t, dir, "show", res.Branch+":ov/kustomization.yaml"); strings.Count(k, "rightsize-deployment-api.yaml") != 1 {
		t.Fatalf("listed twice:\n%s", k)
	}
}

func TestProposeManifestChangesEveryContainerOfACombinedRecord(t *testing.T) {
	dir := repo(t, strings.Replace(manifest, "      - name: sidecar\n        image: busybox\n",
		"      - name: sidecar\n        image: busybox\n        resources:\n          requests:\n            cpu: 500m\n", 1))
	res, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: withSidecar()})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(run(t, dir, "show", res.Branch+":k8s/api.yaml"), `"`, "")
	if !strings.Contains(got, `cpu: 250m`) || !strings.Contains(got, `cpu: 1`) {
		t.Fatalf("%s", got)
	}
	if _, err := Propose(Options{RepoDir: repo(t, manifest), File: "k8s/api.yaml", Rec: withSidecar()}); err == nil || !strings.Contains(err.Error(), "container sidecar") {
		t.Fatalf("a sidecar without the values it was computed for must be refused: %v", err)
	}
}

// --push sends the branch to the remote and --open-pr hands it to the PR opener with the commit's title and body; neither happens
// unless asked, and a PR is never attempted without a push.
func TestProposePushesAndOpensAPullRequestOnlyWhenAsked(t *testing.T) {
	dir := repo(t, manifest)
	remote := t.TempDir()
	run(t, remote, "init", "-q", "--bare")
	run(t, dir, "remote", "add", "origin", remote)
	var gotTitle, gotBody, gotBase string
	opener := func(_, branch, base, title, body string) (string, error) {
		gotTitle, gotBody, gotBase = title, body, base
		return "https://example.test/pr/1", nil
	}
	res, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec(), Push: true, OpenPR: true, Base: "main", PROpener: opener})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.PRURL != "https://example.test/pr/1" || gotBase != "main" || !strings.Contains(gotTitle, "abc123") || !strings.Contains(gotBody, "not a bill") {
		t.Fatalf("%+v %q %q", res, gotTitle, gotBody)
	}
	if run(t, remote, "rev-parse", res.Branch) != res.Commit {
		t.Fatal("the remote must have the branch at the proposal's commit")
	}
	if _, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec(), Branch: "b2", OpenPR: true, PROpener: opener}); err == nil {
		t.Fatal("a pull request without a push must be refused")
	}
	failing := func(_, _, _, _, _ string) (string, error) { return "", errors.New("no gh login") }
	if _, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec(), Branch: "b3", Push: true, OpenPR: true, PROpener: failing}); err == nil ||
		!strings.Contains(err.Error(), "was pushed") {
		t.Fatalf("a failed PR must say the branch is already pushed: %v", err)
	}
	quiet, err := Propose(Options{RepoDir: dir, File: "k8s/api.yaml", Rec: rec(), Branch: "b4"})
	if err != nil || quiet.Pushed || quiet.PRURL != "" {
		t.Fatalf("by default nothing leaves the machine: %v %+v", err, quiet)
	}
}

// A tampered recommendation must not make propose write outside the repository: the kustomize patch name is derived from the
// workload, so a workload of "../../x" would otherwise escape (found in the 2026-11-12 security review).
func TestProposeRefusesPathsOutsideTheRepository(t *testing.T) {
	dir := repo(t, manifestWithSidecarRequests)
	write(t, dir, "overlays/prod/kustomization.yaml", "resources:\n- ../../k8s\n")
	commitAll(t, dir)
	evil := rec()
	evil.Workload = "../../../../escaped"
	if _, err := Propose(Options{RepoDir: dir, File: "overlays/prod/kustomization.yaml", Format: FormatKustomize, Rec: evil, Branch: "evil"}); err == nil {
		t.Fatal("a workload that is not a Kubernetes name must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "escaped")); err == nil {
		t.Fatal("a file was written outside the repository")
	}
	for _, f := range []string{"../outside.yaml", "k8s/../../outside.yaml"} {
		if _, err := Propose(Options{RepoDir: dir, File: f, Rec: rec(), Branch: "evil2"}); err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
