package execute

// These tests use client-go's FAKE clientset (no real API server). They prove the logic and its failure handling;
// the real-cluster run is a separate lab test (hack/lab-e2e.sh).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/safety"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time        { return c.t }
func (c *clock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func dep() *appsv1.Deployment {
	two := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Annotations: map[string]string{AnnApproved: "rec1"}},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("2Gi")}}}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 0, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2},
	}
}

func pod(name string, ready bool, restarts int32, oom bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	cs := corev1.ContainerStatus{Name: "app", RestartCount: restarts}
	if oom {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled"}
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "api"}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}, ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

func rec() recommend.Recommendation {
	return recommend.Recommendation{ID: "rec1", Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Confidence: recommend.ConfHigh,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: 2, Recommended: 1, Limit: 4},
		Memory: recommend.ResourceResult{Action: recommend.Decrease, Current: 2 << 30, Recommended: 1 << 30, Limit: 2 << 30}}
}

func pol() safety.Policy { return safety.Policy{AllowNamespaces: []string{"shop"}} }

type env struct {
	ex  *Executor
	kc  *fake.Clientset
	aud *Memory
	clk *clock
}

func newEnv(objs ...runtime.Object) *env {
	kc := fake.NewSimpleClientset(objs...)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{vpaGVR: "VerticalPodAutoscalerList"})
	aud, clk := &Memory{}, &clock{t: t0}
	return &env{ex: &Executor{KC: kc, Dyn: dyn, Audit: aud, Actor: "tester", Now: clk.now, Sleep: clk.sleep}, kc: kc, aud: aud, clk: clk}
}

func (e *env) container(t *testing.T) corev1.Container {
	t.Helper()
	d, err := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Spec.Containers[0]
}

func cpuReq(c corev1.Container) string {
	q := c.Resources.Requests[corev1.ResourceCPU]
	return q.String()
}

func TestDryRunByDefaultWritesNothing(t *testing.T) {
	e := newEnv(dep())
	res, err := e.ex.Apply(context.Background(), rec(), pol(), false)
	if err != nil || !res.Decision.Allowed || !res.DryRun || res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	if cpuReq(e.container(t)) != "2" {
		t.Fatal("dry-run modified the Deployment")
	}
	for _, a := range e.kc.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "patch" {
			t.Fatalf("unexpected write: %v", a)
		}
	}
	if got := strings.Join(e.aud.Types(), ","); got != "decision,dry_run" {
		t.Fatal(got)
	}
}

func TestApplyRecordsPreviousStateAndIsIdempotent(t *testing.T) {
	e := newEnv(dep())
	res, err := e.ex.Apply(context.Background(), rec(), pol(), true)
	if err != nil || !res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	c := e.container(t)
	if cpuReq(c) != "1" || c.Resources.Requests.Memory().Value() != 1<<30 {
		t.Fatalf("%+v", c.Resources)
	}
	d, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Annotations[AnnApplied] != "rec1" || !strings.Contains(d.Annotations[AnnPrevious], `"cpuRequest":"2"`) || d.Annotations[AnnAppliedAt] == "" {
		t.Fatalf("%v", d.Annotations)
	}
	// the very same recommendation again is refused
	writes := countWrites(e.kc)
	res, err = e.ex.Apply(context.Background(), rec(), pol(), true)
	if err != nil || res.Applied || res.Decision.Allowed || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "already applied") {
		t.Fatalf("%v %+v", err, res)
	}
	if countWrites(e.kc) != writes {
		t.Fatal("a duplicate apply wrote again")
	}
}

func countWrites(kc *fake.Clientset) int {
	n := 0
	for _, a := range kc.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "patch" || a.GetVerb() == "create" || a.GetVerb() == "delete" {
			n++
		}
	}
	return n
}

func TestNoPolicyNoApprovalNoWrite(t *testing.T) {
	e := newEnv(dep())
	res, _ := e.ex.Apply(context.Background(), rec(), safety.Policy{}, true)
	if res.Applied || res.Decision.Allowed || countWrites(e.kc) != 0 {
		t.Fatalf("%+v", res)
	}
	d := dep()
	delete(d.Annotations, AnnApproved)
	e = newEnv(d)
	res, _ = e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !res.Decision.NeedsApproval || countWrites(e.kc) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestStaleRecommendationIsRefused(t *testing.T) {
	d := dep()
	d.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("3")
	e := newEnv(d)
	res, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "stale") {
		t.Fatalf("%+v", res)
	}
}

func TestGitOpsHPAAndVPAAreDetected(t *testing.T) {
	d := dep()
	d.Annotations["argocd.argoproj.io/tracking-id"] = "app:apps/Deployment:shop/api"
	e := newEnv(d)
	res, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || res.Facts.GitOps != "argocd" {
		t.Fatalf("%+v", res)
	}
	d = dep()
	d.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}
	if f, _, _ := newEnv(d).ex.Inspect(context.Background(), "Deployment", "shop", "api"); f.GitOps != "flux" {
		t.Fatal("flux not detected")
	}

	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"},
			Metrics: []autoscalingv2.MetricSpec{{Type: "Resource", Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU}}}}}
	e = newEnv(dep(), h)
	res, _ = e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !res.Facts.HPAOnCPU {
		t.Fatalf("an HPA on CPU must block a CPU change: %+v", res)
	}

}

func TestVPAIsDetectedAndMissingDynamicClientFailsClosed(t *testing.T) {
	e := newEnv(dep())
	vpa := vpaObject("api", "Auto")
	if _, err := e.ex.Dyn.Resource(vpaGVR).Namespace("shop").Create(context.Background(), vpa, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !res.Facts.VPAAuto {
		t.Fatalf("%+v", res)
	}
	e = newEnv(dep())
	e.ex.Dyn = nil
	res, _ = e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || res.Facts.Known {
		t.Fatalf("without a way to rule out a VPA nothing may be applied: %+v", res)
	}
}

func TestRateLimitUsesRecentApplies(t *testing.T) {
	other := dep()
	other.Name = "web"
	other.Annotations = map[string]string{AnnAppliedAt: t0.Add(-5 * time.Minute).Format(time.RFC3339)}
	e := newEnv(dep(), other)
	res, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "rate limit") {
		t.Fatalf("%+v", res)
	}
}

func TestConflictingUpdateChangesNothing(t *testing.T) {
	e := newEnv(dep())
	e.kc.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", errors.New("the object has been modified"))
	})
	res, err := e.ex.Apply(context.Background(), rec(), pol(), true)
	if err == nil || res.Applied || cpuReq(e.container(t)) != "2" {
		t.Fatalf("%v %+v", err, res)
	}
	if !contains(e.aud.Types(), "apply_failed") {
		t.Fatal("failure not audited")
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestJudge(t *testing.T) {
	healthy := Observation{RolloutComplete: true, Replicas: 2, AvailableReplica: 2}
	if r := Judge(healthy, 2, Thresholds{}); len(r) != 0 {
		t.Fatal(r)
	}
	for name, o := range map[string]Observation{
		"oom":       {RolloutComplete: true, Replicas: 2, AvailableReplica: 2, OOMKilled: 1},
		"restart":   {RolloutComplete: true, Replicas: 2, AvailableReplica: 2, Restarts: 1},
		"available": {RolloutComplete: true, Replicas: 2, AvailableReplica: 1},
		"notready":  {RolloutComplete: true, Replicas: 2, AvailableReplica: 2, NotReady: 1},
		"replicas":  {RolloutComplete: true, Replicas: 5, AvailableReplica: 5},
		"slo":       {RolloutComplete: true, Replicas: 2, AvailableReplica: 2, SLOBreached: true},
	} {
		if len(Judge(o, 2, Thresholds{})) == 0 {
			t.Errorf("%s not detected", name)
		}
	}
	if len(Judge(Observation{Restarts: 2, RolloutComplete: true, Replicas: 2, AvailableReplica: 2}, 2, Thresholds{MaxRestarts: 2})) != 0 {
		t.Fatal("configured tolerance ignored")
	}
}

func TestHealthyChangeIsVerifiedAndKept(t *testing.T) {
	e := newEnv(dep(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	res, v, err := e.ex.ApplyWatchRollback(context.Background(), rec(), pol(), Thresholds{ObserveFor: time.Minute, Poll: 10 * time.Second})
	if err != nil || !res.Applied || !v.Healthy {
		t.Fatalf("%v %+v %+v", err, res, v)
	}
	if cpuReq(e.container(t)) != "1" {
		t.Fatal("a healthy change must stay")
	}
	if got := strings.Join(e.aud.Types(), ","); got != "decision,applied,verified" {
		t.Fatal(got)
	}
}

func TestDegradationTriggersVerifiedRollback(t *testing.T) {
	e := newEnv(dep(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	var polls int32
	// after the apply, p2 gets OOMKilled on the second poll
	e.ex.Sleep = func(d time.Duration) {
		e.clk.sleep(d)
		if atomic.AddInt32(&polls, 1) == 2 {
			_ = e.kc.CoreV1().Pods("shop").Delete(context.Background(), "p2", metav1.DeleteOptions{})
			_, _ = e.kc.CoreV1().Pods("shop").Create(context.Background(), pod("p2", true, 1, true), metav1.CreateOptions{})
		}
	}
	res, v, err := e.ex.ApplyWatchRollback(context.Background(), rec(), pol(), Thresholds{ObserveFor: 5 * time.Minute, Poll: 10 * time.Second})
	if err != nil || !res.Applied || v.Healthy || !strings.Contains(strings.Join(v.Reasons, " "), "OOMKilled") {
		t.Fatalf("%v %+v %+v", err, res, v)
	}
	c := e.container(t)
	if cpuReq(c) != "2" || c.Resources.Requests.Memory().Value() != 2<<30 {
		t.Fatalf("not restored: %+v", c.Resources)
	}
	d, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Annotations[AnnRolledBack] != "rec1" || d.Annotations[AnnPrevious] != "" || d.Annotations[AnnApplied] != "" {
		t.Fatalf("%v", d.Annotations)
	}
	if got := strings.Join(e.aud.Types(), ","); got != "decision,applied,degraded,rolled_back" {
		t.Fatal(got)
	}
	// the rolled-back recommendation is never retried
	res2, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res2.Applied || !strings.Contains(strings.Join(res2.Decision.Reasons, " "), "rolled back") {
		t.Fatalf("%+v", res2)
	}
}

func TestSLOBreachRollsBack(t *testing.T) {
	e := newEnv(dep(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	slo := func(context.Context) (bool, bool, error) { return e.clk.t.After(t0.Add(25 * time.Second)), true, nil }
	_, v, err := e.ex.ApplyWatchRollback(context.Background(), rec(), pol(), Thresholds{ObserveFor: 5 * time.Minute, Poll: 10 * time.Second, SLO: slo})
	if err != nil || v.Healthy || cpuReq(e.container(t)) != "2" {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestRolloutTimeoutRollsBack(t *testing.T) {
	d := dep()
	d.Status.AvailableReplicas = 0 // never becomes available
	e := newEnv(d, pod("p1", false, 0, false))
	_, v, err := e.ex.ApplyWatchRollback(context.Background(), rec(), pol(), Thresholds{RolloutTimeout: time.Minute, Poll: 10 * time.Second})
	// the values are restored, but this fake workload never becomes available even then, so the rollback reports it loudly
	if err == nil || !strings.Contains(err.Error(), "restored values are in place") || v.Healthy || !strings.Contains(strings.Join(v.Reasons, " "), "did not complete") || cpuReq(e.container(t)) != "2" {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestRollbackWithoutSavedStateAndFailedRollbackAreErrors(t *testing.T) {
	e := newEnv(dep())
	if err := e.ex.Rollback(context.Background(), "Deployment", "shop", "api", nil, 0); err == nil {
		t.Fatal("nothing to roll back must be an error")
	}
	e = newEnv(dep(), pod("p1", true, 0, true), pod("p2", true, 0, false))
	e.kc.PrependReactor("update", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		d := a.(ktesting.UpdateAction).GetObject().(*appsv1.Deployment)
		if d.Annotations[AnnRolledBack] != "" { // the rollback write fails
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	_, _, err := e.ex.ApplyWatchRollback(context.Background(), rec(), pol(), Thresholds{ObserveFor: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "rollback") || !contains(e.aud.Types(), "rollback_failed") {
		t.Fatalf("a failed rollback must surface loudly: %v %v", err, e.aud.Types())
	}
}

func TestUnsupportedKindAndMissingContainer(t *testing.T) {
	e := newEnv(dep())
	r := rec()
	r.Kind = "StatefulSet"
	if _, err := e.ex.Apply(context.Background(), r, pol(), true); err == nil {
		t.Fatal("unsupported kind accepted")
	}
	r = rec()
	r.Container = "nope"
	res, _ := e.ex.Apply(context.Background(), r, pol(), true)
	if res.Applied {
		t.Fatal("missing container applied")
	}
}

func vpaObject(target, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.k8s.io/v1", "kind": "VerticalPodAutoscaler",
		"metadata": map[string]any{"name": "v-" + target, "namespace": "shop"},
		"spec":     map[string]any{"targetRef": map[string]any{"kind": "Deployment", "name": target}, "updatePolicy": map[string]any{"updateMode": mode}},
	}}
}

func TestLoweringALimitIsRefused(t *testing.T) {
	e := newEnv(dep())
	r := rec()
	r.Memory.Limit = 512 << 20 // live limit is 2Gi
	res, _ := e.ex.Apply(context.Background(), r, pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "lower the memory limit") || countWrites(e.kc) != 0 {
		t.Fatalf("%+v", res)
	}
	d := dep()
	delete(d.Spec.Template.Spec.Containers[0].Resources.Limits, corev1.ResourceCPU)
	e = newEnv(d)
	res, _ = e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "new cpu limit") {
		t.Fatalf("%+v", res)
	}
}

// A real cluster showed this: the deployment controller updates STATUS concurrently, so the first Update after a read can conflict.
func TestRollbackRetriesOnConflict(t *testing.T) {
	e := newEnv(dep(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	if res, err := e.ex.Apply(context.Background(), rec(), pol(), true); err != nil || !res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	var conflicts int32
	e.kc.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&conflicts, 1) <= 2 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", errors.New("modified"))
		}
		return false, nil, nil
	})
	if err := e.ex.Rollback(context.Background(), "Deployment", "shop", "api", []string{"test"}, 0); err != nil {
		t.Fatalf("a conflict on the rollback write must be retried: %v", err)
	}
	if cpuReq(e.container(t)) != "2" || atomic.LoadInt32(&conflicts) != 3 {
		t.Fatalf("restored=%s attempts=%d", cpuReq(e.container(t)), conflicts)
	}
}

func TestApplyRetriesStatusConflictButNotASpecChange(t *testing.T) {
	e := newEnv(dep())
	var n int32
	e.kc.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", errors.New("status updated"))
		}
		return false, nil, nil
	})
	res, err := e.ex.Apply(context.Background(), rec(), pol(), true)
	if err != nil || !res.Applied || cpuReq(e.container(t)) != "1" {
		t.Fatalf("a status-only conflict must be retried: %v %+v", err, res)
	}

	// the spec changes between inspection and write (someone else edited it): abort, change nothing
	e = newEnv(dep())
	e.kc.PrependReactor("get", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		return false, nil, nil
	})
	calls := 0
	e.kc.PrependReactor("get", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls >= 2 { // the second read (write phase) sees a bumped generation
			d := dep()
			d.Generation = 7
			return true, d, nil
		}
		return false, nil, nil
	})
	res, err = e.ex.Apply(context.Background(), rec(), pol(), true)
	if err == nil || res.Applied || !strings.Contains(err.Error(), "spec changed") {
		t.Fatalf("%v %+v", err, res)
	}
}
