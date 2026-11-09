package execute

// StatefulSet and DaemonSet support. Fake clientset only (see execute_test.go); the real-cluster run is hack/lab-e2e.sh.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func sts() *appsv1.StatefulSet {
	d := dep()
	two := int32(2)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Annotations: map[string]string{AnnApproved: "rec1"}},
		Spec:       appsv1.StatefulSetSpec{Replicas: &two, Selector: d.Spec.Selector, Template: d.Spec.Template},
		Status:     appsv1.StatefulSetStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
	}
}

func dset() *appsv1.DaemonSet {
	d := dep()
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Annotations: map[string]string{AnnApproved: "rec1"}},
		Spec:       appsv1.DaemonSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2},
	}
}

func recKind(kind string) recommend.Recommendation {
	r := rec()
	r.Kind = kind
	return r
}

func TestStatefulSetApplyWatchAndRollback(t *testing.T) {
	e := newEnv(sts(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	res, v, err := e.ex.ApplyWatchRollback(context.Background(), recKind("StatefulSet"), pol(), Thresholds{ObserveFor: time.Minute, Poll: 10 * time.Second})
	if err != nil || !res.Applied || !v.Healthy {
		t.Fatalf("%v %+v %+v", err, res, v)
	}
	got, _ := e.kc.AppsV1().StatefulSets("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if q := got.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "1" {
		t.Fatalf("not applied: %s", q.String())
	}
	if err := e.ex.Rollback(context.Background(), "StatefulSet", "shop", "api", []string{"manual"}, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = e.kc.AppsV1().StatefulSets("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if q := got.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "2" || got.Annotations[AnnRolledBack] != "rec1" {
		t.Fatalf("not restored: %s %v", q.String(), got.Annotations)
	}
}

func TestOnDeleteStrategyIsRefused(t *testing.T) {
	s := sts()
	s.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	e := newEnv(s)
	res, _ := e.ex.Apply(context.Background(), recKind("StatefulSet"), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "OnDelete") || countWrites(e.kc) != 0 {
		t.Fatalf("%+v", res)
	}
	d := dset()
	d.Spec.UpdateStrategy.Type = appsv1.OnDeleteDaemonSetStrategyType
	e = newEnv(d)
	res, _ = e.ex.Apply(context.Background(), recKind("DaemonSet"), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "OnDelete") {
		t.Fatalf("%+v", res)
	}
}

func TestDaemonSetApplyIgnoresNodeCountChangesButCatchesRestarts(t *testing.T) {
	e := newEnv(dset(), pod("p1", true, 0, false), pod("p2", true, 0, false))
	var polls int
	e.ex.Sleep = func(d time.Duration) {
		e.clk.sleep(d)
		polls++
		if polls == 2 { // a node joins: the desired count changes, which is not about this change
			ds, _ := e.kc.AppsV1().DaemonSets("shop").Get(context.Background(), "api", metav1.GetOptions{})
			ds.Status.DesiredNumberScheduled, ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable = 3, 3, 3
			_, _ = e.kc.AppsV1().DaemonSets("shop").UpdateStatus(context.Background(), ds, metav1.UpdateOptions{})
		}
	}
	res, v, err := e.ex.ApplyWatchRollback(context.Background(), recKind("DaemonSet"), pol(), Thresholds{ObserveFor: time.Minute, Poll: 10 * time.Second})
	if err != nil || !res.Applied || !v.Healthy {
		t.Fatalf("a node joining must not roll back a DaemonSet change: %v %+v %+v", err, res, v)
	}

	e = newEnv(dset(), pod("p1", true, 0, false), pod("p2", true, 1, true))
	_, v, err = e.ex.ApplyWatchRollback(context.Background(), recKind("DaemonSet"), pol(), Thresholds{ObserveFor: time.Minute, Poll: 10 * time.Second})
	if err != nil || v.Healthy {
		t.Fatalf("an OOMKilled pod must roll the change back: %v %+v", err, v)
	}
	got, _ := e.kc.AppsV1().DaemonSets("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if q := got.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "2" {
		t.Fatalf("not restored: %s", q.String())
	}
}

func TestUnknownKindIsRefused(t *testing.T) {
	e := newEnv(dep())
	if _, err := e.ex.Apply(context.Background(), recKind("CronJob"), pol(), true); err == nil {
		t.Fatal("unsupported kind accepted")
	}
	if _, err := e.ex.get(context.Background(), "Job", "shop", "api"); err == nil {
		t.Fatal("unsupported kind accepted by get")
	}
}

func TestRateLimitSeesAllKinds(t *testing.T) {
	s := sts()
	s.Name = "db"
	s.Annotations = map[string]string{AnnAppliedAt: t0.Add(-5 * time.Minute).Format(time.RFC3339)}
	e := newEnv(dep(), s)
	res, _ := e.ex.Apply(context.Background(), rec(), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "rate limit") {
		t.Fatalf("%+v", res)
	}
}

func fakeIdentity(e *env, name string) {
	e.kc.PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectReview{Status: authv1.SelfSubjectReviewStatus{UserInfo: authv1.UserInfo{Username: name}}}, nil
	})
}

func TestApproveStampsTheIdentityTheServerReportsAndPolicyCanRequireIt(t *testing.T) {
	d := dep()
	delete(d.Annotations, AnnApproved)
	e := newEnv(d)
	fakeIdentity(e, "alice@example.com")
	who, err := e.ex.Approve(context.Background(), rec())
	if err != nil || who != "alice@example.com" {
		t.Fatalf("%v %q", err, who)
	}
	got, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	if got.Annotations[AnnApproved] != "rec1" || got.Annotations[AnnApprovedBy] != "alice@example.com" || got.Annotations[AnnApprovedAt] == "" {
		t.Fatalf("%v", got.Annotations)
	}
	if got.Generation != d.Generation {
		t.Fatal("approving must not change the spec")
	}
	p := pol()
	p.Approvers = []string{"bob@example.com"}
	res, _ := e.ex.Apply(context.Background(), rec(), p, true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "not in approvers") {
		t.Fatalf("%+v", res)
	}
	p.Approvers = []string{"alice@example.com"}
	res, _ = e.ex.Apply(context.Background(), rec(), p, true)
	if !res.Applied {
		t.Fatalf("%+v", res)
	}
	if !contains(e.aud.Types(), "approved") {
		t.Fatal("approval not audited")
	}
}

func TestApproveRefusalsAndUnknownIdentity(t *testing.T) {
	e := newEnv(dep())
	// no identity from the server: fail closed
	e.kc.PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	if _, err := e.ex.Approve(context.Background(), rec()); err == nil || !strings.Contains(err.Error(), "who you are") {
		t.Fatalf("%v", err)
	}
	e = newEnv(dep())
	fakeIdentity(e, "")
	if _, err := e.ex.Approve(context.Background(), rec()); err == nil {
		t.Fatal("an empty username was accepted")
	}
	e = newEnv(dep())
	fakeIdentity(e, "alice")
	r := rec()
	r.Abstained = true
	if _, err := e.ex.Approve(context.Background(), r); err == nil {
		t.Fatal("approved an abstention")
	}
	r = rec()
	r.CPU.Current = 9
	if _, err := e.ex.Approve(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("approved a stale recommendation: %v", err)
	}
}
