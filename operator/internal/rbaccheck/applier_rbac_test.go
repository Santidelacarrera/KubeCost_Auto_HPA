package rbaccheck

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type nsDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules []struct {
		APIGroups     []string `json:"apiGroups"`
		Resources     []string `json:"resources"`
		ResourceNames []string `json:"resourceNames"`
		Verbs         []string `json:"verbs"`
	} `json:"rules"`
	Data map[string]string `json:"data"`
}

func parseDocs(t *testing.T, manifest string) []nsDoc {
	t.Helper()
	var out []nsDoc
	for _, part := range strings.Split(manifest, "\n---") {
		var d nsDoc
		if err := yaml.Unmarshal([]byte(part), &d); err != nil {
			t.Fatalf("parsing manifest: %v", err)
		}
		if d.Kind != "" {
			out = append(out, d)
		}
	}
	return out
}

func grants(d nsDoc, group, resource, verb string) bool {
	for _, r := range d.Rules {
		g, res, v := false, false, false
		for _, x := range r.APIGroups {
			g = g || x == group
		}
		for _, x := range r.Resources {
			res = res || x == resource
		}
		for _, x := range r.Verbs {
			v = v || x == verb
		}
		if g && res && v {
			return true
		}
	}
	return false
}

// The recommendation applier changes workload requests, so it must be invisible unless asked for.
func TestApplierIsAbsentByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "applier") || strings.Contains(out, "enable-recommendation-applier") {
		t.Fatal("the default install must contain nothing of the applier")
	}
	for _, d := range parseDocs(t, out) {
		if d.Kind != "ClusterRole" && d.Kind != "Role" {
			continue
		}
		for _, res := range []string{"deployments", "statefulsets", "daemonsets"} {
			if grants(d, "apps", res, "update") {
				t.Errorf("%s %s grants update on %s by default", d.Kind, d.Metadata.Name, res)
			}
		}
		if grants(d, "autoscaling.kubecost.io", "workloadrecommendations/status", "update") {
			t.Errorf("%s %s can write recommendation status by default", d.Kind, d.Metadata.Name)
		}
	}
}

func TestApplierWithNamespacesGrantsWriteOnlyInThoseNamespaces(t *testing.T) {
	docs := parseDocs(t, render(t, "--set", "operator.recommendationApplier.enabled=true", "--set", "operator.recommendationApplier.namespaces={shop,dev}"))
	seen := map[string]bool{}
	for _, d := range docs {
		for _, res := range []string{"deployments", "statefulsets", "daemonsets"} {
			if d.Kind == "ClusterRole" && grants(d, "apps", res, "update") {
				t.Errorf("ClusterRole %s grants cluster-wide update on %s although namespaces were given", d.Metadata.Name, res)
			}
			if d.Kind == "Role" && grants(d, "apps", res, "update") {
				seen[d.Metadata.Namespace] = true
			}
		}
		if !strings.Contains(d.Metadata.Name, "applier") {
			continue // the pre-existing operator roles are covered by the allow-list tests above
		}
		for _, r := range d.Rules {
			for _, v := range r.Verbs {
				if v == "*" || v == "delete" || v == "create" || v == "patch" || v == "escalate" || v == "bind" || v == "impersonate" {
					t.Errorf("%s %s grants %q on %v", d.Kind, d.Metadata.Name, v, r.Resources)
				}
			}
			for _, res := range r.Resources {
				if res == "secrets" || res == "*" || res == "pods/exec" {
					t.Errorf("%s %s must not touch %q", d.Kind, d.Metadata.Name, res)
				}
			}
		}
	}
	if !seen["shop"] || !seen["dev"] || len(seen) != 2 {
		t.Fatalf("workload write must exist as a Role exactly in shop and dev: %v", seen)
	}
}

func TestApplierWithoutNamespacesIsClusterWideAndSaysSo(t *testing.T) {
	docs := parseDocs(t, render(t, "--set", "operator.recommendationApplier.enabled=true"))
	found := false
	for _, d := range docs {
		if d.Kind == "ClusterRole" && strings.HasSuffix(d.Metadata.Name, "applier-workloads") && grants(d, "apps", "deployments", "update") {
			found = true
		}
	}
	if !found {
		t.Fatal("with no namespaces list the grant is cluster-wide; it must at least be a distinct, named ClusterRole")
	}
}

func TestApplierPolicyDefaultAuthorisesNothingAndTheConfigMapReadIsByName(t *testing.T) {
	docs := parseDocs(t, render(t, "--set", "operator.recommendationApplier.enabled=true"))
	var cm, role bool
	for _, d := range docs {
		if d.Kind == "ConfigMap" && strings.HasSuffix(d.Metadata.Name, "applier-policy") {
			cm = strings.Contains(d.Data["policy.yaml"], "allowNamespaces: []")
		}
		if d.Kind == "Role" && strings.HasSuffix(d.Metadata.Name, "applier-policy") {
			role = len(d.Rules) == 1 && len(d.Rules[0].ResourceNames) == 1 && d.Rules[0].Verbs[0] == "get" && len(d.Rules[0].Verbs) == 1
		}
	}
	if !cm || !role {
		t.Fatalf("default policy must authorise no namespace (%v) and the ConfigMap Role must be get-by-name only (%v)", cm, role)
	}
}

func TestApplierAndObserveOnlyAreRenderedIntoTheOperatorArgs(t *testing.T) {
	out := render(t, "--set", "operator.recommendationApplier.enabled=true")
	if !strings.Contains(out, "--enable-recommendation-applier=true") || !strings.Contains(out, "--observe-only=true") {
		t.Fatal("enabling the applier must not switch observe-only off")
	}
}
