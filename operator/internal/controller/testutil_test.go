package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/config"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

type fakeMetrics struct {
	snap  metrics.Snapshot
	err   error
	calls int
}

func (f *fakeMetrics) Snapshot(context.Context, metrics.Workload) (metrics.Snapshot, error) {
	f.calls++
	return f.snap, f.err
}

type fakeEngine struct {
	dec  engine.Decision
	last engine.Request
	err  error
}

func (f *fakeEngine) Evaluate(_ context.Context, r engine.Request) (engine.Decision, error) {
	f.last = r
	return f.dec, f.err
}

type env struct {
	r  *KubeCostHPAReconciler
	cs *fake.Clientset
	m  *fakeMetrics
	e  *fakeEngine
	c  client.Client
	t  time.Time
}

func newDeployment() *appsv1.Deployment {
	lbl := map[string]string{"app": "api"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}}},
		},
	}
}

func newPod(name string) *corev1.Pod {
	d := newDeployment()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: d.Spec.Template.Labels},
		Spec:       *d.Spec.Template.Spec.DeepCopy(),
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func newHPA() *v1alpha1.KubeCostHPA {
	return &v1alpha1.KubeCostHPA{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Generation: 1},
		Spec: v1alpha1.KubeCostHPASpec{
			TargetRef: v1alpha1.TargetRef{Kind: "Deployment", Name: "api"},
			SLO:       v1alpha1.SLO{P99LatencyMs: 200},
			CPU:       v1alpha1.CPUBounds{Min: resource.MustParse("100m"), Max: resource.MustParse("4")},
			Cooldown:  metav1.Duration{Duration: 5 * time.Minute},
			Interval:  metav1.Duration{Duration: time.Minute},
		},
	}
}

func newEnv(hpa *v1alpha1.KubeCostHPA, objs ...runtime.Object) *env {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	cobjs := []client.Object{hpa}
	for _, o := range objs {
		cobjs = append(cobjs, o.(client.Object))
	}
	c := crfake.NewClientBuilder().WithScheme(s).WithObjects(cobjs...).WithStatusSubresource(&v1alpha1.KubeCostHPA{}).Build()
	var kobjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *v1alpha1.KubeCostAutoscaler, *v1alpha1.KubeCostHPA: // CRs are not known to the typed clientset
			continue
		}
		kobjs = append(kobjs, o.DeepCopyObject())
	}
	cs := fake.NewSimpleClientset(kobjs...)
	fakeScale(cs)
	e := &env{cs: cs, c: c, t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		m: &fakeMetrics{snap: metrics.Snapshot{RPS: 200, P99Ms: 50}},
		e: &fakeEngine{dec: engine.Decision{Action: "shrink", CPUMillicores: 800, Reason: "slack"}}}
	store := cache.NewMemory()
	store.Now = func() time.Time { return e.t }
	e.r = &KubeCostHPAReconciler{Client: c, Resizer: &actuator.Resizer{Client: cs}, Metrics: e.m, Engine: e.e,
		Store: store, Recorder: record.NewFakeRecorder(50), Defaults: config.Defaults{CPUPricePerCoreHour: 0.03, MemPricePerGiBHour: 0.004},
		Now: func() time.Time { return e.t }}
	return e
}

// syncPods mirrors pods from the clientset (where resizes land) into the
// controller-runtime fake client (what the reconciler reads), emulating the
// informer cache catching up.
func (e *env) syncPods() {
	ctx := context.Background()
	pods, _ := e.cs.CoreV1().Pods("shop").List(ctx, metav1.ListOptions{})
	for _, p := range pods.Items {
		var cur corev1.Pod
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(&p), &cur); err != nil {
			continue
		}
		cur.Spec.Containers = p.Spec.Containers
		_ = e.c.Update(ctx, &cur)
	}
}

// The client-go fake does not model the scale subresource; emulate it on the Deployment object.
func fakeScale(cs *fake.Clientset) {
	cs.PrependReactor("get", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		o, err := cs.Tracker().Get(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, a.GetNamespace(), a.(clienttesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		d := o.(*appsv1.Deployment)
		n := int32(1)
		if d.Spec.Replicas != nil {
			n = *d.Spec.Replicas
		}
		return true, &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}, Spec: autoscalingv1.ScaleSpec{Replicas: n}}, nil
	})
	cs.PrependReactor("update", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		sc := a.(clienttesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		o, err := cs.Tracker().Get(gvr, sc.Namespace, sc.Name)
		if err != nil {
			return true, nil, err
		}
		d := o.(*appsv1.Deployment).DeepCopy()
		d.Spec.Replicas = &sc.Spec.Replicas
		return true, sc, cs.Tracker().Update(gvr, d, sc.Namespace)
	})
}
