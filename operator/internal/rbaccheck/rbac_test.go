package rbaccheck

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type rule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type doc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Rules []rule `json:"rules"`
}

// flatten turns rules into "group/resource:verb" entries ("core" for the empty group).
func flatten(rules []rule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		for _, g := range r.APIGroups {
			if g == "" {
				g = "core"
			}
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out[g+"/"+res+":"+v] = true
				}
			}
		}
	}
	return out
}

// wantAll is the complete permission set with Spot draining enabled (the default). Each entry
// is justified in docs/RBAC.md with the code that needs it.
var wantAll = []string{
	"core/events:create", "core/events:patch",
	"core/nodes:get", "core/nodes:list", "core/nodes:watch", "core/nodes:patch",
	"core/pods:get", "core/pods:list", "core/pods:watch",
	"core/pods/eviction:create",
	"core/pods/resize:patch",
	"apps/deployments:get", "apps/deployments:list", "apps/deployments:watch",
	"apps/deployments/scale:get", "apps/deployments/scale:update",
	"autoscaling/horizontalpodautoscalers:create", "autoscaling/horizontalpodautoscalers:delete",
	"autoscaling/horizontalpodautoscalers:get", "autoscaling/horizontalpodautoscalers:list",
	"autoscaling/horizontalpodautoscalers:patch", "autoscaling/horizontalpodautoscalers:watch",
	"autoscaling.kubecost.io/kubecostautoscalers:get", "autoscaling.kubecost.io/kubecostautoscalers:list",
	"autoscaling.kubecost.io/kubecostautoscalers:watch",
	"autoscaling.kubecost.io/kubecostautoscalers/status:get", "autoscaling.kubecost.io/kubecostautoscalers/status:update",
	"autoscaling.kubecost.io/kubecosthpas:get", "autoscaling.kubecost.io/kubecosthpas:list",
	"autoscaling.kubecost.io/kubecosthpas:update", "autoscaling.kubecost.io/kubecosthpas:watch",
	"autoscaling.kubecost.io/kubecosthpas/finalizers:update",
	"autoscaling.kubecost.io/kubecosthpas/status:get", "autoscaling.kubecost.io/kubecosthpas/status:update",
	"metrics.k8s.io/pods:list",
	"policy/poddisruptionbudgets:get", "policy/poddisruptionbudgets:list", "policy/poddisruptionbudgets:watch",
}

// spotOnly is what only the Spot drain controller (--enable-spot-drain) needs.
var spotOnly = []string{"core/nodes:patch", "core/pods/eviction:create"}

func set(items []string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

func diff(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()
	var extra, missing []string
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 || len(missing) > 0 {
		t.Errorf("%s differs from the allow-list.\n  granted but not allow-listed (privilege added?): %v\n  allow-listed but not granted (feature broken?): %v\n  Change internal/rbaccheck and docs/RBAC.md deliberately if this is intended.", what, extra, missing)
	}
}

func clusterRoles(t *testing.T, manifest string) []doc {
	t.Helper()
	var out []doc
	for _, part := range strings.Split(manifest, "\n---") {
		var d doc
		if err := yaml.Unmarshal([]byte(part), &d); err != nil {
			t.Fatalf("parsing manifest: %v", err)
		}
		if d.Kind == "ClusterRole" {
			out = append(out, d)
		}
	}
	return out
}

func neverGranted(t *testing.T, what string, rules []rule) {
	t.Helper()
	forbiddenRes := map[string]bool{"secrets": true, "configmaps": true, "pods/exec": true, "pods/attach": true, "pods/portforward": true,
		"serviceaccounts": true, "serviceaccounts/token": true, "roles": true, "rolebindings": true, "clusterroles": true,
		"clusterrolebindings": true, "namespaces": true, "persistentvolumes": true, "customresourcedefinitions": true,
		"mutatingwebhookconfigurations": true, "validatingwebhookconfigurations": true, "tokenreviews": true, "daemonsets": true,
		"statefulsets": true, "jobs": true, "cronjobs": true, "ingresses": true, "services": true}
	forbiddenVerbs := map[string]bool{"*": true, "impersonate": true, "escalate": true, "bind": true, "deletecollection": true}
	for _, r := range rules {
		for _, g := range r.APIGroups {
			if g == "*" {
				t.Errorf("%s: wildcard API group", what)
			}
		}
		for _, res := range r.Resources {
			if res == "*" || forbiddenRes[res] {
				t.Errorf("%s: must not grant access to %q", what, res)
			}
		}
		for _, v := range r.Verbs {
			if forbiddenVerbs[v] {
				t.Errorf("%s: must not grant verb %q", what, v)
			}
		}
	}
}

func TestGeneratedClusterRoleEqualsTheAllowList(t *testing.T) {
	b, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	roles := clusterRoles(t, string(b))
	if len(roles) != 1 {
		t.Fatalf("want exactly one ClusterRole in role.yaml, got %d", len(roles))
	}
	diff(t, "config/rbac/role.yaml", flatten(roles[0].Rules), set(wantAll))
	neverGranted(t, "config/rbac/role.yaml", roles[0].Rules)
}

func render(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("REQUIRE_HELM") == "1" {
			t.Fatal("helm is required (REQUIRE_HELM=1) but not on PATH")
		}
		t.Skip("helm not installed; the Helm half of this check runs in CI")
	}
	out, err := exec.Command("helm", append([]string{"template", "r", "../../../deploy/helm/kubecost-auto-hpa"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

func TestHelmClusterRoleMatchesTheGeneratedRole(t *testing.T) {
	roles := clusterRoles(t, render(t))
	if len(roles) != 1 {
		t.Fatalf("want one ClusterRole in the chart, got %d", len(roles))
	}
	diff(t, "Helm ClusterRole (defaults)", flatten(roles[0].Rules), set(wantAll))
	neverGranted(t, "Helm ClusterRole (defaults)", roles[0].Rules)
}

func TestHelmWithoutSpotDrainDropsNodePatchAndEviction(t *testing.T) {
	roles := clusterRoles(t, render(t, "--set", "operator.enableSpotDrain=false"))
	if len(roles) != 1 {
		t.Fatalf("want one ClusterRole in the chart, got %d", len(roles))
	}
	want := set(wantAll)
	for _, k := range spotOnly {
		delete(want, k)
	}
	diff(t, "Helm ClusterRole (enableSpotDrain=false)", flatten(roles[0].Rules), want)
	for _, k := range spotOnly {
		if flatten(roles[0].Rules)[k] {
			t.Errorf("%s must not be granted without Spot draining", k)
		}
	}
}

// Leader election needs leases and events in the release namespace only: it must stay a Role.
func TestLeaderElectionIsNamespacedAndNarrow(t *testing.T) {
	out := render(t)
	n := 0
	for _, part := range strings.Split(out, "\n---") {
		var d doc
		if err := yaml.Unmarshal([]byte(part), &d); err != nil || d.Kind != "Role" {
			continue
		}
		n++
		got := flatten(d.Rules)
		for k := range got {
			if !strings.HasPrefix(k, "coordination.k8s.io/leases:") && !strings.HasPrefix(k, "core/events:") {
				t.Errorf("leader-election Role grants unexpected %s", k)
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one namespaced Role, got %d", n)
	}
}
