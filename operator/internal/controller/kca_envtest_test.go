//go:build envtest

package controller

// KubeCostAutoscaler against a REAL kube-apiserver 1.33 + etcd: CRD defaults and
// CEL validation, the scale subresource, status subresource, native HPA
// validation and the single-writer guarantee. Cost and resource metrics are
// fakes (there is no KubeCost or metrics-server in envtest).

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/kubecost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/ownership"
)

func kcaObject(name string, spec map[string]any) *unstructured.Unstructured {
	base := map[string]any{"targetRef": map[string]any{"name": "payments-api"}, "minReplicas": int64(2), "maxReplicas": int64(20)}
	for k, v := range spec {
		base[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "KubeCostAutoscaler",
		"metadata": map[string]any{"name": name, "namespace": "production"}, "spec": base}}
}

func TestRealAPIServer_KubeCostAutoscalerSchema(t *testing.T) {
	c, _ := startEnv(t)
	ctx := context.Background()
	mustNoErr(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "production"}}))

	// defaults
	mustNoErr(t, c.Create(ctx, kcaObject("minimal", nil)))
	var got v1alpha1.KubeCostAutoscaler
	mustNoErr(t, c.Get(ctx, types.NamespacedName{Namespace: "production", Name: "minimal"}, &got))
	s := got.Spec
	if s.TargetRef.Kind != "Deployment" || s.TargetRef.APIVersion != "apps/v1" || s.Demand.TargetUtilizationPercent != 70 || s.Demand.CeilingUtilizationPercent != 90 ||
		s.Demand.CostPressureUtilizationPercent != 80 || s.Demand.TolerancePercent == nil || *s.Demand.TolerancePercent != 10 || len(s.Demand.Resources) != 1 ||
		s.Stabilization.DownscaleWindow.Duration != 10*time.Minute || s.Stabilization.ScaleUpWindow.Duration != 30*time.Second ||
		s.Stabilization.MinimumScaleActionInterval.Duration != time.Minute || s.FailurePolicy.Mode != "Freeze" ||
		s.FailurePolicy.MaxMetricAge.Duration != 5*time.Minute || s.FailurePolicy.AllowSafetyScaleUp == nil || !*s.FailurePolicy.AllowSafetyScaleUp ||
		s.InfrastructurePolicy.SpotMultiplier != "1.0" || s.EvaluationInterval.Duration != 30*time.Second || s.DryRun {
		t.Fatalf("defaults not applied: %+v", s)
	}

	// the illustrative manifest from the design must be accepted as is
	mustNoErr(t, c.Create(ctx, kcaObject("full", map[string]any{
		"costPolicy":           map[string]any{"maxHourlyCostUSD": "5.00", "targetHourlyCostUSD": "4.00", "scaleUpCostGuardrail": true},
		"stabilization":        map[string]any{"downscaleWindow": "10m", "scaleUpWindow": "30s", "minimumScaleActionInterval": "60s"},
		"failurePolicy":        map[string]any{"mode": "ResourceMetricsFallback", "maxMetricAge": "2m"},
		"infrastructurePolicy": map[string]any{"spotMultiplier": "0.35", "onDemandMultiplier": "1.0"},
	})))

	// the shipped sample must be accepted by the real schema
	raw, err := os.ReadFile("../../config/samples/autoscaling_v1alpha1_kubecostautoscaler.yaml")
	mustNoErr(t, err)
	sample := &unstructured.Unstructured{}
	mustNoErr(t, yaml.Unmarshal(raw, &sample.Object))
	mustNoErr(t, c.Create(ctx, sample))

	for name, spec := range map[string]map[string]any{
		"max<min":          {"minReplicas": int64(5), "maxReplicas": int64(2)},
		"min zero":         {"minReplicas": int64(0)},
		"minHealthy>max":   {"minHealthyReplicas": int64(50)},
		"wrong kind":       {"targetRef": map[string]any{"name": "x", "kind": "StatefulSet"}},
		"wrong apiVersion": {"targetRef": map[string]any{"name": "x", "apiVersion": "apps/v2"}},
		"empty name":       {"targetRef": map[string]any{"name": ""}},
		"util order":       {"demand": map[string]any{"targetUtilizationPercent": int64(95)}},
		"util > 100":       {"demand": map[string]any{"ceilingUtilizationPercent": int64(120)}},
		"bad resource":     {"demand": map[string]any{"resources": []any{"gpu"}}},
		"bad budget":       {"costPolicy": map[string]any{"maxHourlyCostUSD": "five"}},
		"negative budget":  {"costPolicy": map[string]any{"maxHourlyCostUSD": "-5"}},
		"bad mode":         {"failurePolicy": map[string]any{"mode": "Yolo"}},
		"bad multiplier":   {"infrastructurePolicy": map[string]any{"spotMultiplier": "cheap"}},
		"buffer > 90":      {"infrastructurePolicy": map[string]any{"spotInterruptionBufferPercent": int64(95)}},
		"tolerance >= 50":  {"demand": map[string]any{"tolerancePercent": int64(50)}},
		"empty label key":  {"infrastructurePolicy": map[string]any{"spotNodeLabels": []any{map[string]any{"key": ""}}}},
		"fallback cpu = 0": {"failurePolicy": map[string]any{"fallbackTargetCPUUtilizationPercent": int64(0)}},
	} {
		if err := c.Create(ctx, kcaObject("bad-"+sanitize(name), spec)); err == nil {
			t.Errorf("%s: invalid object was accepted", name)
		}
	}

	// the target of an existing autoscaler cannot be re-pointed
	var cur v1alpha1.KubeCostAutoscaler
	mustNoErr(t, c.Get(ctx, types.NamespacedName{Namespace: "production", Name: "minimal"}, &cur))
	cur.Spec.TargetRef.Name = "something-else"
	if err := c.Update(ctx, &cur); err == nil {
		t.Fatal("targetRef must be immutable")
	}
}

func sanitize(s string) string {
	out := []rune{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// realEnv wires the reconciler to the real API server with fake cost/metrics.
type realEnv struct {
	t     *testing.T
	c     client.Client
	cs    *kubernetes.Clientset
	r     *KubeCostAutoscalerReconciler
	cost  *fakeCost
	res   *fakeResources
	now   time.Time
	price string
}

func newRealEnv(t *testing.T, spec map[string]any, replicas int32) *realEnv {
	t.Helper()
	c, cs, env, adminCfg := startEnvFull(t)
	ctx := context.Background()
	mustNoErr(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "production"}}))
	lbl := map[string]string{"app": "payments-api"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-api", Namespace: "production"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}}},
	}
	mustNoErr(t, c.Create(ctx, dep))
	mustNoErr(t, c.Get(ctx, client.ObjectKeyFromObject(dep), dep))
	dep.Status.Replicas, dep.Status.ReadyReplicas = replicas, replicas // no deployment controller in envtest
	mustNoErr(t, c.Status().Update(ctx, dep))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "od-1", Labels: map[string]string{"karpenter.sh/capacity-type": "on-demand"}}}
	mustNoErr(t, c.Create(ctx, node))
	for i := int32(0); i < replicas; i++ {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-api-" + string(rune('a'+i)), Namespace: "production", Labels: lbl},
			Spec: corev1.PodSpec{NodeName: "od-1", Containers: []corev1.Container{{Name: "app", Image: "nginx",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}}},
		}
		mustNoErr(t, c.Create(ctx, p))
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		mustNoErr(t, c.Status().Update(ctx, p))
	}
	mustNoErr(t, c.Create(ctx, kcaObject("payments-autoscaler", spec)))

	e := &realEnv{t: t, c: c, cs: cs, now: time.Now(), price: "0.5"}
	now := func() time.Time { return e.now }
	e.cost = &fakeCost{nowFunc: now, fn: func(n time.Time) (kubecost.Sample, error) {
		var d appsv1.Deployment
		_ = c.Get(ctx, client.ObjectKeyFromObject(dep), &d)
		return validSample(n, hourly(e.price, *d.Spec.Replicas)), nil
	}}
	e.res = &fakeResources{util: 0.7}
	s := c.Scheme()
	// The reconciler runs as a user that holds only the generated ClusterRole (config/rbac/role.yaml),
	// not cluster-admin; the test itself keeps the admin client `c` for setup and assertions. Every
	// real-apiserver scenario below therefore also proves the permissions are sufficient.
	user, err := restrictedOperator(env, adminCfg)
	mustNoErr(t, err)
	ucfg := user.Config()
	rc, err := client.New(ucfg, client.Options{Scheme: s})
	mustNoErr(t, err)
	rcs, err := kubernetes.NewForConfig(ucfg)
	mustNoErr(t, err)
	e.r = &KubeCostAutoscalerReconciler{
		Client: rc, Scheme: s, Cost: e.cost, Resources: e.res, Scaler: &actuator.DeploymentScaler{Client: rcs},
		Fallback: &ownership.Fallback{Client: rc, Scheme: s}, Recorder: record.NewFakeRecorder(1000), Now: now, Jitter: func() float64 { return 0 },
	}
	return e
}

func hourly(perReplica string, n int32) string {
	v := validSample(time.Now(), perReplica).HourlyCost.MulInt(int64(n))
	return v.String()
}

func (e *realEnv) reconcile() {
	e.t.Helper()
	if _, err := e.r.Reconcile(context.Background(), kcaReq); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
}

func (e *realEnv) replicas() int32 {
	var d appsv1.Deployment
	mustNoErr(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "payments-api"}, &d))
	return *d.Spec.Replicas
}

func (e *realEnv) status() v1alpha1.KubeCostAutoscalerStatus {
	var a v1alpha1.KubeCostAutoscaler
	mustNoErr(e.t, e.c.Get(context.Background(), kcaReq.NamespacedName, &a))
	return a.Status
}

func TestRealAPIServer_ScalesThroughTheScaleSubresourceAndWritesStatus(t *testing.T) {
	e := newRealEnv(t, map[string]any{"costPolicy": map[string]any{"maxHourlyCostUSD": "5.00", "targetHourlyCostUSD": "4.00"}}, 4)
	e.res.util = 0.97
	e.reconcile()
	if got := e.replicas(); got < 5 {
		t.Fatalf("replicas=%d: expected a safety scale-up through the real scale subresource", got)
	}
	st := e.status()
	if st.LastScaleTime == nil || st.LastDecision == nil || st.LastDecision.Type != "ScaleUp" || len(st.Conditions) != 4 || st.Cost == nil || len(st.Recommendations) != 1 ||
		st.ObservedGeneration != 1 || st.ScalingOwner != v1alpha1.OwnerOperator {
		t.Fatalf("status rejected or incomplete: %+v", st)
	}
	// compare-and-swap: a concurrent change by someone else is never overwritten
	dep := &appsv1.Deployment{}
	mustNoErr(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: "payments-api"}, dep))
	other := int32(9)
	dep.Spec.Replicas = &other
	mustNoErr(t, e.c.Update(context.Background(), dep))
	err := (&actuator.DeploymentScaler{Client: e.cs}).Scale(context.Background(), "production", "payments-api", 5, 6)
	if err == nil || e.replicas() != 9 {
		t.Fatalf("stale write must be refused (err=%v replicas=%d)", err, e.replicas())
	}
}

func TestRealAPIServer_NativeHPAConflictBlocksAllWrites(t *testing.T) {
	e := newRealEnv(t, nil, 4)
	ctx := context.Background()
	min := int32(1)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "team-hpa", Namespace: "production"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 8,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "payments-api"},
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: int32Ptr(60)}}}}},
	}
	mustNoErr(t, e.c.Create(ctx, hpa))
	for i := 0; i < 5; i++ {
		e.res.util = 0.99
		e.reconcile()
		e.now = e.now.Add(time.Minute)
	}
	if e.replicas() != 4 {
		t.Fatalf("an active HPA exists: replicas must not be written (got %d)", e.replicas())
	}
	st := e.status()
	if st.ScalingOwner != v1alpha1.OwnerNone {
		t.Fatalf("%+v", st)
	}
	// once the HPA is gone the autoscaler takes over
	mustNoErr(t, e.c.Delete(ctx, hpa))
	e.reconcile()
	if e.replicas() <= 4 {
		t.Fatalf("expected the autoscaler to resume, replicas=%d", e.replicas())
	}
}

func TestRealAPIServer_FallbackHPAIsValidOwnedAndRemovedOnRecovery(t *testing.T) {
	e := newRealEnv(t, map[string]any{
		"costPolicy":    map[string]any{"maxHourlyCostUSD": "5.00"},
		"failurePolicy": map[string]any{"mode": "ResourceMetricsFallback"},
	}, 4)
	failing := true
	orig := e.cost.fn
	e.cost.fn = func(n time.Time) (kubecost.Sample, error) {
		if failing {
			return kubecost.Sample{}, transientErr()
		}
		return orig(n)
	}
	for i := 0; i < 12; i++ {
		e.now = e.now.Add(20 * time.Second)
		e.r.forgetCost(kcaReq.NamespacedName)
		e.reconcile()
	}
	var h autoscalingv2.HorizontalPodAutoscaler
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: ownership.HPAName("payments-autoscaler")}, &h); err != nil {
		t.Fatalf("the real API server must accept the fallback HPA: %v", err)
	}
	if !metav1.IsControlledBy(&h, ptrObj(func() v1alpha1.KubeCostAutoscaler {
		var a v1alpha1.KubeCostAutoscaler
		_ = e.c.Get(context.Background(), kcaReq.NamespacedName, &a)
		return a
	}())) {
		t.Fatalf("the fallback HPA must be controlled by the autoscaler: %+v", h.OwnerReferences)
	}
	if st := e.status(); !st.Fallback.Active || st.ScalingOwner != v1alpha1.OwnerFallbackHPA {
		t.Fatalf("%+v", st)
	}
	failing = false
	for i := 0; i < 12; i++ {
		e.now = e.now.Add(20 * time.Second)
		e.r.forgetCost(kcaReq.NamespacedName)
		e.reconcile()
	}
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "production", Name: ownership.HPAName("payments-autoscaler")}, &h)
	if err == nil {
		t.Fatal("fallback HPA must be deleted after recovery")
	}
}

func int32Ptr(v int32) *int32 { return &v }
