package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/kubecost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/ownership"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/resmetrics"
)

var kcaReq = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "production", Name: "payments-autoscaler"}}

// ---- fakes ----

type fakeCost struct {
	mu      sync.Mutex
	calls   int
	fn      func(now time.Time) (kubecost.Sample, error)
	nowFunc func() time.Time
}

func (f *fakeCost) Allocation(_ context.Context, q kubecost.Query) (kubecost.Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.fn(f.nowFunc())
}

type fakeResources struct {
	util     float64
	err      error
	availErr error
}

func (f *fakeResources) Utilization(context.Context, string, labels.Selector, []corev1.Pod, []corev1.ResourceName, time.Time, time.Duration) (resmetrics.Utilization, error) {
	if f.err != nil {
		return resmetrics.Utilization{}, f.err
	}
	return resmetrics.Utilization{Value: f.util, Resource: corev1.ResourceCPU, Pods: 1}, nil
}
func (f *fakeResources) Available(context.Context) error { return f.availErr }

type write struct {
	At       time.Time
	From, To int32
}

// fakeScaler mimics DeploymentScaler (compare-and-swap) on the fake client and
// records every write.
type fakeScaler struct {
	c      client.Client
	now    func() time.Time
	writes []write
	fail   error
}

func (s *fakeScaler) Scale(ctx context.Context, ns, name string, from, to int32) error {
	if s.fail != nil {
		return s.fail
	}
	var d appsv1.Deployment
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
		return err
	}
	if cur := *d.Spec.Replicas; cur != from {
		return fmt.Errorf("%w: expected %d, found %d", actuator.ErrReplicaDrift, from, cur)
	}
	d.Spec.Replicas = &to
	d.Status.ReadyReplicas = to
	if err := s.c.Update(ctx, &d); err != nil {
		return err
	}
	s.writes = append(s.writes, write{At: s.now(), From: from, To: to})
	return nil
}

// ---- environment ----

type kcaEnv struct {
	t    *testing.T
	r    *KubeCostAutoscalerReconciler
	c    client.Client
	cost *fakeCost
	res  *fakeResources
	scl  *fakeScaler
	rec  *record.FakeRecorder
	now  time.Time

	pricePerReplica string
	costErr         error
	costMod         func(*kubecost.Sample)
}

type envOpt func(*kcaEnv, *[]client.Object, *interceptor.Funcs)

func validSample(now time.Time, hourly string) kubecost.Sample {
	h, _ := money.Parse(hourly)
	return kubecost.Sample{TotalCost: h.MulRat(15, 60), HourlyCost: h, Minutes: 15, WindowEnd: now.Add(-10 * time.Second), Window: 15 * time.Minute}
}

func baseAutoscaler() *v1alpha1.KubeCostAutoscaler {
	return &v1alpha1.KubeCostAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-autoscaler", Namespace: "production", Generation: 1, UID: "uid-kca",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))},
		Spec: v1alpha1.KubeCostAutoscalerSpec{
			TargetRef:   v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "payments-api"},
			MinReplicas: 2, MaxReplicas: 20,
			CostPolicy: &v1alpha1.CostPolicy{MaxHourlyCostUSD: "5.00", TargetHourlyCostUSD: "4.00"},
		},
	}
}

func paymentsDeployment(replicas int32) *appsv1.Deployment {
	lbl := map[string]string{"app": "payments-api"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-api", Namespace: "production", UID: "uid-dep"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}}},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

func paymentsPods(n int, node string) []client.Object {
	var out []client.Object
	for i := 0; i < n; i++ {
		out = append(out, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("payments-api-%d", i), Namespace: "production", Labels: map[string]string{"app": "payments-api"}},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		})
	}
	return out
}

func onDemandNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"karpenter.sh/capacity-type": "on-demand"}}}
}
func spotNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"karpenter.sh/capacity-type": "spot"}}}
}

func newKCAEnv(t *testing.T, a *v1alpha1.KubeCostAutoscaler, replicas int32, opts ...envOpt) *kcaEnv {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	e := &kcaEnv{t: t, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), pricePerReplica: "0.5"}
	objs := []client.Object{a, paymentsDeployment(replicas), onDemandNode("od-1"), spotNode("spot-1")}
	objs = append(objs, paymentsPods(int(replicas), "od-1")...)
	var icept interceptor.Funcs
	for _, o := range opts {
		o(e, &objs, &icept)
	}
	e.c = crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.KubeCostAutoscaler{}, &v1alpha1.KubeCostHPA{}).WithInterceptorFuncs(icept).Build()
	now := func() time.Time { return e.now }
	e.cost = &fakeCost{nowFunc: now, fn: func(n time.Time) (kubecost.Sample, error) {
		if e.costErr != nil {
			return kubecost.Sample{}, e.costErr
		}
		var dep appsv1.Deployment
		_ = e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "payments-api"}, &dep)
		p, _ := money.Parse(e.pricePerReplica)
		smp := validSample(n, p.MulInt(int64(*dep.Spec.Replicas)).String())
		if e.costMod != nil {
			e.costMod(&smp)
		}
		return smp, nil
	}}
	e.res = &fakeResources{util: 0.7}
	e.scl = &fakeScaler{c: e.c, now: now}
	e.rec = record.NewFakeRecorder(500)
	e.r = &KubeCostAutoscalerReconciler{
		Client: e.c, Scheme: s, Cost: e.cost, Resources: e.res, Scaler: e.scl,
		Fallback: &ownership.Fallback{Client: e.c, Scheme: s}, Recorder: e.rec, Now: now, Jitter: func() float64 { return 0 },
	}
	return e
}

func (e *kcaEnv) reconcile() (ctrl.Result, error) {
	e.t.Helper()
	return e.r.Reconcile(context.Background(), kcaReq)
}

func (e *kcaEnv) mustReconcile() ctrl.Result {
	e.t.Helper()
	res, err := e.reconcile()
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *kcaEnv) advance(d time.Duration) {
	e.now = e.now.Add(d)
	e.r.forgetCost(kcaReq.NamespacedName)
}

func (e *kcaEnv) get() v1alpha1.KubeCostAutoscaler {
	e.t.Helper()
	var a v1alpha1.KubeCostAutoscaler
	if err := e.c.Get(context.Background(), kcaReq.NamespacedName, &a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *kcaEnv) replicas() int32 {
	var d appsv1.Deployment
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "payments-api"}, &d); err != nil {
		e.t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func (e *kcaEnv) cond(typ string) *metav1.Condition {
	a := e.get()
	return meta.FindStatusCondition(a.Status.Conditions, typ)
}

func (e *kcaEnv) requireCond(typ string, status metav1.ConditionStatus, reason string) {
	e.t.Helper()
	c := e.cond(typ)
	if c == nil || c.Status != status || (reason != "" && c.Reason != reason) {
		e.t.Fatalf("condition %s = %+v, want %s/%s", typ, c, status, reason)
	}
}

func (e *kcaEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (e *kcaEnv) hpa() (*autoscalingv2.HorizontalPodAutoscaler, bool) {
	var h autoscalingv2.HorizontalPodAutoscaler
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: ownership.HPAName("payments-autoscaler")}, &h)
	return &h, err == nil
}

// restart simulates a controller restart: all in-memory state is gone, only
// the objects in the API server (the fake client) survive.
func (e *kcaEnv) restart() {
	e.r = &KubeCostAutoscalerReconciler{
		Client: e.c, Scheme: e.r.Scheme, Cost: e.cost, Resources: e.res, Scaler: e.scl,
		Fallback: e.r.Fallback, Recorder: e.rec, Now: e.r.Now, Jitter: e.r.Jitter,
	}
}

func transientErr() error {
	return &kubecost.Error{Class: kubecost.ClassTransient, Err: errors.New("connection refused")}
}

type reconcileRequest = ctrl.Request
