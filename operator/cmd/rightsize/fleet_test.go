package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func wrec(ns, name, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "WorkloadRecommendation",
		"metadata": map[string]any{"namespace": ns, "name": name}}}
	if phase != "" {
		u.Object["status"] = map[string]any{"phase": phase}
	}
	return u
}

func TestFleetSummaryOfOneCluster(t *testing.T) {
	n1, n2 := invNode("n1", "us-central1-a"), invNode("n2", "us-central1-b")
	n1.Spec.ProviderID = "gce://proj/us-central1-a/n1"
	kc := fake.NewSimpleClientset(n1, n2, invPod("a", "n1", nil), invPod("b", "n2", nil),
		invPod("waiting", "", func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}}
		}))
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{wrecGVR: "WorkloadRecommendationList"},
		wrec("shop", "r1", "Verified"), wrec("shop", "r2", ""), wrec("shop", "r3", "RolledBack"), wrec("pay", "r4", "Pending"))

	s := summariseCluster(context.Background(), "gke-prod", kc, dyn, 0.85)
	if !s.Reachable || s.Provider != "gce" || s.Nodes != 2 || len(s.Zones) != 2 {
		t.Fatalf("%+v", s)
	}
	if s.CPUAllocatableCores != 8 || s.CPURequestedCores != 1 || s.CPURequestedPct != 12.5 || s.MemAllocatableGiB != 32 || s.MemRequestedGiB != 2 {
		t.Fatalf("capacity: %+v", s)
	}
	if s.PendingUnschedulable != 1 {
		t.Fatalf("pending: %+v", s)
	}
	if s.Recommendations["Pending"] != 2 || s.Recommendations["Verified"] != 1 || s.Recommendations["RolledBack"] != 1 {
		t.Fatalf("an object without a phase is Pending: %v", s.Recommendations)
	}
	// two zones, one node each: neither may be removed (zone availability), so no candidate is claimed
	if len(s.RemovableCandidates) != 0 {
		t.Fatalf("%v", s.RemovableCandidates)
	}
}

func TestFleetTotalsCoverOnlyReachableClusters(t *testing.T) {
	r := FleetReport{Clusters: []ClusterSummary{
		{Context: "a", Reachable: true, Nodes: 3, CPUAllocatableCores: 12, CPURequestedCores: 6, RemovableCandidates: []string{"n3"}, Recommendations: map[string]int{"Pending": 2}},
		{Context: "b", Reachable: true, Nodes: 2, CPUAllocatableCores: 8, CPURequestedCores: 7},
		{Context: "c", Error: "connection refused", Nodes: 99},
	}}
	fleetTotals(&r)
	if r.Totals.Clusters != 3 || r.Totals.Reachable != 2 || r.Totals.Nodes != 5 || r.Totals.CPUAllocatableCores != 20 || r.Totals.RemovableCandidates != 1 || r.Totals.PendingRecommendations != 2 {
		t.Fatalf("%+v", r.Totals)
	}
	notes := strings.Join(r.Notes, " ")
	if !strings.Contains(notes, "1 cluster(s) could not be read") || !strings.Contains(notes, "not savings") {
		t.Fatalf("%v", r.Notes)
	}
}

func TestFleetWithoutTheCRDStillReports(t *testing.T) {
	s := summariseCluster(context.Background(), "plain", fake.NewSimpleClientset(invNode("n1", "")), nil, 0.85)
	if !s.Reachable || s.Recommendations != nil || s.Provider != "unknown" {
		t.Fatalf("%+v", s)
	}
}

func TestFleetNeedsExplicitContexts(t *testing.T) {
	if err := fleetCmd(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "--contexts") {
		t.Fatalf("%v", err)
	}
}

func TestLicenseIssueAndVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "vendor")
	if err := keygenCmd([]string{"--out", prefix}, io.Discard); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "acme.license")
	if err := licenseCmd([]string{"issue", "--key", prefix + ".key", "--id", "L-7", "--customer", "acme", "--tier", "professional", "--max-nodes", "40", "--out", file}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := licenseCmd([]string{"verify", "--file", file, "--public-key", prefix + ".pub"}, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Valid   bool
		Mode    string
		License struct {
			Tier     string
			MaxNodes int
		}
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil || !got.Valid || got.License.Tier != "professional" || got.License.MaxNodes != 40 || !strings.Contains(got.Mode, "human approval") {
		t.Fatalf("%v %s", err, out.String())
	}
	// refuses to overwrite, refuses an unknown tier, and a licence does not verify with another vendor's key
	if err := licenseCmd([]string{"issue", "--key", prefix + ".key", "--id", "L-8", "--customer", "acme", "--tier", "enterprise", "--out", file}, io.Discard); err == nil {
		t.Fatal("overwrote a licence")
	}
	if err := licenseCmd([]string{"issue", "--key", prefix + ".key", "--id", "L-9", "--customer", "acme", "--tier", "gold", "--out", filepath.Join(dir, "x")}, io.Discard); err == nil {
		t.Fatal("unknown tier accepted")
	}
	other := filepath.Join(dir, "other")
	if err := keygenCmd([]string{"--out", other}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := licenseCmd([]string{"verify", "--file", file, "--public-key", other + ".pub"}, io.Discard); err == nil {
		t.Fatal("verified with the wrong key")
	}
}

func TestRenderCopiesFiguresLabelsLevelsAndEscapesClusterText(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	rec := write("rec.json", `{"ID":"abc","Namespace":"shop","Workload":"api|x","Kind":"Deployment","Container":"<b>app</b>","Confidence":"High",
	  "CPU":{"Action":"Decrease","Current":1,"Recommended":0.5,"PotentialMonthlyUSD":11.5},"Memory":{"Action":"NoChange","Current":1073741824,"Recommended":1073741824},
	  "Risks":["line one\nline two"],"Insights":{"Signals":["cpu-spikes"]}}`)
	nodes := write("nodes.json", `{"Analysis":{"Nodes":[{"Name":"n1","CPUPct":0.25,"MemPct":0.5,"Pods":3,"Removable":true,"Why":"its pods fit elsewhere"}],"Removable":["n1"],"CandidateHourlyUSD":0.2,
	  "Caveats":["a removable node is a CANDIDATE"],"Signals":{}},"Plan":{"Steps":[{"Node":"n1","Commands":["kubectl cordon n1","kubectl drain n1"],"Undo":["kubectl uncordon n1"]}]}}`)
	bills := write("bills.json", `{"Level":"observed","MissingForNext":["the invoiced amount did not fall"],"Reconciliation":{"Currency":"USD","Comparable":true,"Basis":"BilledCost","SavingPerDay":0,
	  "ListSavingPerDay":5000000,"Before":{"Start":"2026-10-01T00:00:00Z","End":"2026-10-08T00:00:00Z"},"After":{"Start":"2026-10-08T00:00:00Z","End":"2026-10-15T00:00:00Z"},"Notes":["credits absorb it"]}}`)
	var out strings.Builder
	if err := renderCmd([]string{"--plan", rec + "," + filepath.Join(dir, "missing.json"), "--nodes", nodes, "--reconcile", bills, "--title", "Test"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"# Test", "Decrease: 1000m to 500m", "11.50", "cpu-spikes", "Level: *estimated*", // the figure and its level, copied
		`shop/Deployment/api\|x`, "&lt;b&gt;app&lt;/b&gt;", "line one line two", // text from the cluster cannot break the table or inject markup
		"**yes**", "A **candidate**, not a saving", "`kubectl drain n1`", "undo: `kubectl uncordon n1`", "nothing executes it",
		"Level of the saving: **observed**", "the invoiced amount did not fall", "5.000000 USD per day",
		"Input not included", "missing.json", // an unreadable input is named, not dropped
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<b>") {
		t.Error("markup from an input reached the report")
	}
	if err := renderCmd(nil, io.Discard); err == nil {
		t.Error("no input accepted")
	}
}

func TestRenderRefusesToShowASavingForBillsThatCannotBeCompared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	body := `{"Level":"projected","Reconciliation":{"Currency":"USD","Comparable":false,"NotComparable":["different billing currencies (USD and EUR)"],"SavingPerDay":0}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := renderCmd([]string{"--reconcile", path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cannot be compared") || strings.Contains(out.String(), "per day") {
		t.Fatalf("%s", out.String())
	}
}

func TestSurveyTargetsAndOneFailureDoesNotStopTheRest(t *testing.T) {
	mk := func(containers ...string) corev1.PodTemplateSpec {
		var cs []corev1.Container
		for _, c := range containers {
			cs = append(cs, corev1.Container{Name: c})
		}
		return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: cs}}
	}
	kc := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}, Spec: appsv1.DeploymentSpec{Template: mk("app", "proxy")}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"}, Spec: appsv1.DeploymentSpec{Template: mk("app")}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "x"}, Spec: appsv1.DeploymentSpec{Template: mk("app")}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db"}, Spec: appsv1.StatefulSetSpec{Template: mk("pg")}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "agent"}, Spec: appsv1.DaemonSetSpec{Template: mk("a")}},
	)
	targets, err := surveyTargets(context.Background(), kc, "shop")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range targets {
		names = append(names, x.Kind+"/"+x.Name+":"+strings.Join(x.Containers, "+"))
	}
	if got := strings.Join(names, " "); got != "Deployment/api:app Deployment/web:app+proxy StatefulSet/db:pg" {
		t.Fatalf("sorted, this namespace only, no DaemonSet: %s", got)
	}
	var calls []string
	recs, problems := runSurvey(targets, func(kind, name, container string, w io.Writer) error {
		calls = append(calls, name+"/"+container)
		switch {
		case name == "api":
			return fmt.Errorf("prometheus unreachable")
		case container == "proxy":
			_, err := w.Write([]byte(`{"not":"a record"}`))
			return err
		}
		_, err := fmt.Fprintf(w, `{"ID":"id-%s","Namespace":"shop","Workload":%q,"Kind":%q,"Container":%q}`, name, name, kind, container)
		return err
	})
	if len(calls) != 4 {
		t.Fatalf("every container of every workload is planned, whatever failed before: %v", calls)
	}
	if len(recs) != 2 || recs[0].Workload != "web" || recs[1].Workload != "db" {
		t.Fatalf("%+v", recs)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "Deployment/api container app: prometheus unreachable") || !strings.Contains(problems[1], "did not return a recommendation") {
		t.Fatalf("%v", problems)
	}
}

func TestSurveyGuardsItsFlags(t *testing.T) {
	if err := surveyCmd([]string{"--namespace", "shop"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--context") {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"--workload=x", "--publish"} {
		if err := surveyCmd([]string{"--context", "c", "--namespace", "shop", bad}, io.Discard); err == nil || !strings.Contains(err.Error(), "not a survey flag") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}
