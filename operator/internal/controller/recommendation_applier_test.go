package controller

// Fake controller-runtime client and fake clientset: logic and failure handling only. The real-cluster run is hack/lab-applier.sh.

import (
	"context"
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

var tApp = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type appClock struct{ t time.Time }

func (c *appClock) now() time.Time        { return c.t }
func (c *appClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func appDeployment(ns string) *appsv1.Deployment {
	two := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: ns},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("2Gi")}}}}}}},
		Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2},
	}
}

func appPod(name, ns string, restarts int32, oom bool) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "app", RestartCount: restarts}
	if oom {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled"}
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "api"}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

func appRec(ns string) recommend.Recommendation {
	return recommend.Recommendation{ID: "rec1", Namespace: ns, Workload: "api", Kind: "Deployment", Container: "app", Confidence: recommend.ConfHigh,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: 2, Recommended: 1, Limit: 4},
		Memory: recommend.ResourceResult{Action: recommend.Decrease, Current: 2 << 30, Recommended: 1 << 30, Limit: 2 << 30}}
}

func appCR(ns string, rec recommend.Recommendation, approved bool) *v1alpha1.WorkloadRecommendation {
	raw, _ := json.Marshal(rec)
	cr := &v1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Name: "api-rec1", Namespace: ns},
		Spec: v1alpha1.WorkloadRecommendationSpec{RecommendationID: rec.ID, Record: string(raw),
			Target: v1alpha1.RecommendationTarget{Kind: rec.Kind, Name: rec.Workload, Container: rec.Container}},
	}
	if approved {
		cr.Status.Approval = &v1alpha1.RecommendationApproval{RecommendationID: rec.ID, By: "alice@example.com", At: metav1.NewTime(tApp)}
	}
	return cr
}

func policyCM(policy string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "applier-policy", Namespace: "kubecost-auto-hpa-system"}, Data: map[string]string{PolicyKey: policy}}
}

type appEnv struct {
	r   *RecommendationApplier
	c   client.Client
	kc  *k8sfake.Clientset
	clk *appClock
	rec *record.FakeRecorder
}

func newAppEnv(t *testing.T, observeOnly bool, crs []client.Object, kobjs ...runtime.Object) *appEnv {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	var crObjs []client.Object
	for _, o := range crs {
		if cm, ok := o.(*corev1.ConfigMap); ok {
			kobjs = append(kobjs, cm) // the policy is read with a direct GET through the clientset, like in the real operator
			continue
		}
		crObjs = append(crObjs, o)
	}
	c := crfake.NewClientBuilder().WithScheme(s).WithObjects(crObjs...).WithStatusSubresource(&v1alpha1.WorkloadRecommendation{}).Build()
	kc := k8sfake.NewSimpleClientset(kobjs...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}: "VerticalPodAutoscalerList"})
	clk, rec := &appClock{t: tApp}, record.NewFakeRecorder(200)
	return &appEnv{c: c, kc: kc, clk: clk, rec: rec, r: &RecommendationApplier{Client: c, KC: kc, Dyn: dyn, Recorder: rec, PolicyNamespace: "kubecost-auto-hpa-system", PolicyName: "applier-policy",
		ObserveOnly: observeOnly, MaxObserve: time.Minute, MaxRollout: time.Minute, Now: clk.now, Sleep: clk.sleep}}
}

func (e *appEnv) reconcile(t *testing.T, ns, name string) ctrl.Result {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *appEnv) phase(t *testing.T, ns, name string) v1alpha1.WorkloadRecommendation {
	t.Helper()
	var cr v1alpha1.WorkloadRecommendation
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

func (e *appEnv) cpu(t *testing.T, ns string) string {
	t.Helper()
	d, err := e.kc.AppsV1().Deployments(ns).Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	q := d.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	return q.String()
}

const okPolicy = "allowNamespaces: [shop]\n"

func healthy(ns string) []runtime.Object {
	return []runtime.Object{appDeployment(ns), appPod("p1", ns, 0, false), appPod("p2", ns, 0, false)}
}

func TestWithoutApprovalNothingHappensEvenIfTheWorkloadIsAnnotatedAsApproved(t *testing.T) {
	d := appDeployment("shop")
	d.Annotations = map[string]string{execute.AnnApproved: "rec1", execute.AnnApprovedBy: "mallory"} // forged on the workload
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), false), policyCM(okPolicy)}, d, appPod("p1", "shop", 0, false))
	e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhasePending || e.cpu(t, "shop") != "2" {
		t.Fatalf("%s %s", cr.Status.Phase, e.cpu(t, "shop"))
	}
}

func TestAnApprovalOfAnotherRecommendationIsIgnored(t *testing.T) {
	cr := appCR("shop", appRec("shop"), true)
	cr.Status.Approval.RecommendationID = "other"
	e := newAppEnv(t, false, []client.Object{cr, policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if got := e.phase(t, "shop", "api-rec1").Status.Phase; got != v1alpha1.RecPhasePending || e.cpu(t, "shop") != "2" {
		t.Fatalf("%s", got)
	}
}

func TestNoPolicyConfigMapMeansNoChange(t *testing.T) {
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true)}, healthy("shop")...)
	res := e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhaseDenied || !strings.Contains(cr.Status.Message, "no usable policy") || e.cpu(t, "shop") != "2" || res.RequeueAfter == 0 {
		t.Fatalf("%+v %v", cr.Status, res)
	}
	// a malformed policy (typo in a key) is also a refusal, not a guess
	e = newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), policyCM("allowNamespace: [shop]\n")}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if e.phase(t, "shop", "api-rec1").Status.Phase != v1alpha1.RecPhaseDenied || e.cpu(t, "shop") != "2" {
		t.Fatal("a policy with an unknown key must deny")
	}
}

func TestARecommendationCannotActOutsideItsOwnNamespace(t *testing.T) {
	rec := appRec("prod") // the object lives in "shop" but the record says "prod"
	e := newAppEnv(t, false, []client.Object{appCR("shop", rec, true), policyCM("allowNamespaces: [shop, prod]\n")}, append(healthy("prod"), healthy("shop")[1:]...)...)
	e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhaseDenied || !strings.Contains(cr.Status.Message, "own namespace") || e.cpu(t, "prod") != "2" {
		t.Fatalf("%+v", cr.Status)
	}
	// a record whose ID differs from the object's
	cr2 := appCR("shop", appRec("shop"), true)
	cr2.Spec.RecommendationID = "something-else"
	e = newAppEnv(t, false, []client.Object{cr2, policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if e.phase(t, "shop", "api-rec1").Status.Phase != v1alpha1.RecPhaseDenied || e.cpu(t, "shop") != "2" {
		t.Fatal("an ID mismatch must deny")
	}
}

func TestObserveOnlyReportsDryRunAndWritesNothing(t *testing.T) {
	e := newAppEnv(t, true, []client.Object{appCR("shop", appRec("shop"), true), policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if got := e.phase(t, "shop", "api-rec1").Status.Phase; got != v1alpha1.RecPhaseDryRun || e.cpu(t, "shop") != "2" {
		t.Fatalf("%s", got)
	}
	for _, a := range e.kc.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "patch" {
			t.Fatalf("observe-only wrote: %v", a)
		}
	}
}

func TestApprovedAndAllowedIsAppliedWatchedAndVerified(t *testing.T) {
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhaseVerified || e.cpu(t, "shop") != "1" || cr.Status.AppliedAt == nil || cr.Status.FinishedAt == nil {
		t.Fatalf("%+v cpu=%s", cr.Status, e.cpu(t, "shop"))
	}
	d, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Annotations[execute.AnnApplied] != "rec1" || d.Annotations[execute.AnnPrevious] == "" {
		t.Fatalf("the previous state must be recorded on the workload: %v", d.Annotations)
	}
	// terminal: another reconcile does nothing
	writes := countKCWrites(e.kc)
	e.reconcile(t, "shop", "api-rec1")
	if countKCWrites(e.kc) != writes {
		t.Fatal("a Verified recommendation was touched again")
	}
}

func countKCWrites(kc *k8sfake.Clientset) int {
	n := 0
	for _, a := range kc.Actions() {
		switch a.GetVerb() {
		case "update", "patch", "create", "delete":
			n++
		}
	}
	return n
}

func TestDegradationRollsBackAndRecordsIt(t *testing.T) {
	objs := []runtime.Object{appDeployment("shop"), appPod("p1", "shop", 0, false), appPod("p2", "shop", 1, true)}
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), policyCM(okPolicy)}, objs...)
	e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhaseRolledBack || e.cpu(t, "shop") != "2" || !strings.Contains(strings.Join(cr.Status.Reasons, " "), "OOMKilled") {
		t.Fatalf("%+v cpu=%s", cr.Status, e.cpu(t, "shop"))
	}
	d, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Annotations[execute.AnnRolledBack] != "rec1" {
		t.Fatalf("%v", d.Annotations)
	}
}

func TestThePolicyIsReadAgainSoTheKillSwitchActsWithoutARestart(t *testing.T) {
	cm := policyCM("allowNamespaces: [shop]\nkillSwitch: true\n")
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), cm}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	cr := e.phase(t, "shop", "api-rec1")
	if cr.Status.Phase != v1alpha1.RecPhaseDenied || !strings.Contains(strings.Join(cr.Status.Reasons, " "), "kill switch") || e.cpu(t, "shop") != "2" {
		t.Fatalf("%+v", cr.Status)
	}
	// the operator lifts the switch in the ConfigMap; the next look applies it
	cur, err := e.kc.CoreV1().ConfigMaps(cm.Namespace).Get(context.Background(), cm.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Data[PolicyKey] = okPolicy
	if _, err := e.kc.CoreV1().ConfigMaps(cm.Namespace).Update(context.Background(), cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, "shop", "api-rec1")
	if e.phase(t, "shop", "api-rec1").Status.Phase != v1alpha1.RecPhaseVerified || e.cpu(t, "shop") != "1" {
		t.Fatal("the change should apply once the policy allows it")
	}
}

func TestAnInterruptedRunIsResumedNotRepeated(t *testing.T) {
	// the first process applied the change and died while watching: the workload carries the recorded state
	d := appDeployment("shop")
	d.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
	d.Annotations = map[string]string{execute.AnnApplied: "rec1", execute.AnnPrevious: `{"recommendationId":"rec1","containers":{"app":{"cpuRequest":"2","cpuLimit":"4","memRequest":"2Gi","memLimit":"2Gi"}}}`}
	cr := appCR("shop", appRec("shop"), true)
	cr.Status.Phase = v1alpha1.RecPhaseApplying
	e := newAppEnv(t, false, []client.Object{cr, policyCM(okPolicy)}, d, appPod("p1", "shop", 0, false), appPod("p2", "shop", 0, false))
	e.reconcile(t, "shop", "api-rec1")
	if got := e.phase(t, "shop", "api-rec1").Status.Phase; got != v1alpha1.RecPhaseVerified {
		t.Fatalf("%s", got)
	}
	if n := countKCWrites(e.kc); n != 0 {
		t.Fatalf("resuming must not write to the workload again (%d writes)", n)
	}
}

func TestShutdownDuringObservationDoesNotRollBack(t *testing.T) {
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), policyCM(okPolicy)}, healthy("shop")...)
	ctx, cancel := context.WithCancel(context.Background())
	e.r.Sleep = func(d time.Duration) { e.clk.sleep(d); cancel() } // the operator is stopped during the first pause
	res, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "api-rec1"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("%v %v", err, res)
	}
	d, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Annotations[execute.AnnRolledBack] != "" || e.cpu(t, "shop") != "1" {
		t.Fatalf("a shutdown is not evidence of a degradation: %v cpu=%s", d.Annotations, e.cpu(t, "shop"))
	}
	if got := e.phase(t, "shop", "api-rec1").Status.Phase; got != v1alpha1.RecPhaseApplying {
		t.Fatalf("the phase must stay Applying so the next process resumes: %s", got)
	}
}

func TestUnknownSLOQueryNameIsRefused(t *testing.T) {
	cr := appCR("shop", appRec("shop"), true)
	cr.Spec.SLOQueryName = "latency"
	e := newAppEnv(t, false, []client.Object{cr, policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if got := e.phase(t, "shop", "api-rec1"); got.Status.Phase != v1alpha1.RecPhaseDenied || !strings.Contains(got.Status.Message, "not defined") || e.cpu(t, "shop") != "2" {
		t.Fatalf("%+v", got.Status)
	}
}

// Found on a real cluster: a queued reconcile saw a cached copy one update behind (still Applying), re-evaluated the change that had
// already ended and overwrote the final RolledBack with Denied.
func TestAFinishedOutcomeIsNeverOverwrittenEvenWithAStaleCache(t *testing.T) {
	stale := appCR("shop", appRec("shop"), true)
	stale.Status.Phase = v1alpha1.RecPhaseApplying // what the cache still shows
	e := newAppEnv(t, false, []client.Object{stale, policyCM(okPolicy)}, healthy("shop")...)

	live := appCR("shop", appRec("shop"), true)
	live.Status.Phase, live.Status.Message = v1alpha1.RecPhaseRolledBack, "degraded after the change and restored"
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	e.r.APIReader = crfake.NewClientBuilder().WithScheme(s).WithObjects(live).WithStatusSubresource(&v1alpha1.WorkloadRecommendation{}).Build()

	e.reconcile(t, "shop", "api-rec1")
	if countKCWrites(e.kc) != 0 {
		t.Fatal("a recommendation whose live phase is final must not be touched")
	}
	// the cached object was never rewritten either
	if got := e.phase(t, "shop", "api-rec1").Status.Phase; got != v1alpha1.RecPhaseApplying {
		t.Fatalf("the reconcile wrote to the cache-side object: %s", got)
	}
}

func TestSetRefusesToLeaveAFinalPhase(t *testing.T) {
	cr := appCR("shop", appRec("shop"), true)
	cr.Status.Phase = v1alpha1.RecPhaseRolledBack
	e := newAppEnv(t, false, []client.Object{cr, policyCM(okPolicy)}, healthy("shop")...)
	var cur v1alpha1.WorkloadRecommendation
	_ = e.c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "api-rec1"}, &cur)
	if _, err := e.r.set(context.Background(), &cur, v1alpha1.RecPhaseDenied, "late and wrong", nil, false); err != nil {
		t.Fatal(err)
	}
	if got := e.phase(t, "shop", "api-rec1"); got.Status.Phase != v1alpha1.RecPhaseRolledBack || got.Status.Message == "late and wrong" {
		t.Fatalf("%+v", got.Status)
	}
	// writing the same final phase again is allowed (idempotent)
	if _, err := e.r.set(context.Background(), &cur, v1alpha1.RecPhaseRolledBack, "again", nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestAnamespaceOwnPolicyReplacesTheGlobalOne(t *testing.T) {
	cm := policyCM("allowNamespaces: [shop]\nmaxChangeFraction: 0.9\n")
	cm.Data["policy.shop.yaml"] = "allowNamespaces: [shop]\nmaxChangeFraction: 0.1\n" // the shop team allows only 10% steps
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), cm}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	got := e.phase(t, "shop", "api-rec1")
	if got.Status.Phase != v1alpha1.RecPhaseDenied || !strings.Contains(strings.Join(got.Status.Reasons, " "), "maxChangeFraction") || e.cpu(t, "shop") != "2" {
		t.Fatalf("the namespace policy (10%%) must win over the global one (90%%): %+v", got.Status)
	}
	// a namespace with no policy of its own falls back to the global key
	cm2 := policyCM("allowNamespaces: [shop]\n")
	cm2.Data["policy.other.yaml"] = "allowNamespaces: [other]\nkillSwitch: true\n"
	e = newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), cm2}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if e.phase(t, "shop", "api-rec1").Status.Phase != v1alpha1.RecPhaseVerified {
		t.Fatal("another namespace's policy must not affect shop")
	}
}

func TestAMalformedNamespacePolicyDeniesRatherThanFallingBack(t *testing.T) {
	cm := policyCM(okPolicy)
	cm.Data["policy.shop.yaml"] = "allowNamespace: [shop]\n" // typo in the key
	e := newAppEnv(t, false, []client.Object{appCR("shop", appRec("shop"), true), cm}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if e.phase(t, "shop", "api-rec1").Status.Phase != v1alpha1.RecPhaseDenied || e.cpu(t, "shop") != "2" {
		t.Fatal("a broken team policy must deny, not silently use the global one")
	}
}

func TestMetricsCountPhasesAndSeparateKeptFromRolledBackSavings(t *testing.T) {
	rec := appRec("shop")
	rec.CPU.PotentialMonthlyUSD, rec.Memory.PotentialMonthlyUSD = 10, 2
	kept0 := promValue(t, "kept")
	e := newAppEnv(t, false, []client.Object{appCR("shop", rec, true), policyCM(okPolicy)}, healthy("shop")...)
	e.reconcile(t, "shop", "api-rec1")
	if got := promValue(t, "kept") - kept0; got != 12 {
		t.Fatalf("kept estimated saving should grow by 12, grew by %v", got)
	}
	rb0 := promValue(t, "rolled_back")
	objs := []runtime.Object{appDeployment("shop"), appPod("p1", "shop", 0, false), appPod("p2", "shop", 1, true)}
	e = newAppEnv(t, false, []client.Object{appCR("shop", rec, true), policyCM(okPolicy)}, objs...)
	e.reconcile(t, "shop", "api-rec1")
	if got := promValue(t, "rolled_back") - rb0; got != 12 {
		t.Fatalf("rolled-back estimated saving should grow by 12, grew by %v", got)
	}
}

func promValue(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(mRecSavings.WithLabelValues(outcome))
}
