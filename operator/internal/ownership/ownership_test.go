package ownership

import (
	"context"
	"errors"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func autoscaler(name string) *v1alpha1.KubeCostAutoscaler {
	return &v1alpha1.KubeCostAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", UID: types.UID("uid-" + name)},
		Spec: v1alpha1.KubeCostAutoscalerSpec{TargetRef: v1alpha1.WorkloadRef{Name: "api"}, MinReplicas: 2, MaxReplicas: 9,
			FailurePolicy: v1alpha1.FailurePolicy{FallbackTargetCPUUtilizationPercent: 65}},
	}
}

func TestHPANameIsDeterministicBoundedAndCollisionResistant(t *testing.T) {
	if HPAName("payments") != "kca-payments" {
		t.Fatal(HPAName("payments"))
	}
	long1 := strings.Repeat("a", 200)
	long2 := strings.Repeat("a", 199) + "b"
	n1, n2 := HPAName(long1), HPAName(long2)
	if len(n1) > 63 || len(n2) > 63 || n1 == n2 || n1 != HPAName(long1) {
		t.Fatalf("%q %q", n1, n2)
	}
}

func TestFallbackLifecycle(t *testing.T) {
	a := autoscaler("pay")
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(a).Build()
	f := &Fallback{Client: c, Scheme: scheme()}
	ctx := context.Background()

	created, err := f.Ensure(ctx, a, 65)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	h, found, err := f.Get(ctx, a)
	if err != nil || !found {
		t.Fatal(err)
	}
	if *h.Spec.MinReplicas != 2 || h.Spec.MaxReplicas != 9 || *h.Spec.Metrics[0].Resource.Target.AverageUtilization != 65 ||
		h.Labels[ManagedByLabel] == "" || !isControlledBy(h, a) {
		t.Fatalf("%+v", h)
	}

	// drift (someone edits the HPA) or a changed autoscaler spec is corrected, not duplicated
	a.Spec.MaxReplicas = 12
	created, err = f.Ensure(ctx, a, 50)
	if err != nil || created {
		t.Fatalf("second Ensure must update: created=%v err=%v", created, err)
	}
	h, _, _ = f.Get(ctx, a)
	if h.Spec.MaxReplicas != 12 || *h.Spec.Metrics[0].Resource.Target.AverageUtilization != 50 {
		t.Fatalf("%+v", h.Spec)
	}

	removed, err := f.Remove(ctx, a)
	if err != nil || !removed {
		t.Fatal(err)
	}
	if _, found, _ := f.Get(ctx, a); found {
		t.Fatal("still there")
	}
	if removed, err := f.Remove(ctx, a); err != nil || removed {
		t.Fatal("removing twice is a no-op")
	}
}

func TestFallbackNeverTouchesForeignObjects(t *testing.T) {
	a := autoscaler("pay")
	foreign := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: HPAName("pay"), Namespace: "prod"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3, ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "x"}}}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(a, foreign).Build()
	f := &Fallback{Client: c, Scheme: scheme()}
	if _, err := f.Ensure(context.Background(), a, 70); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("got %v", err)
	}
	if removed, _ := f.Remove(context.Background(), a); removed {
		t.Fatal("a foreign object must never be deleted")
	}
	var got autoscalingv2.HorizontalPodAutoscaler
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &got)
	if got.Spec.MaxReplicas != 3 {
		t.Fatal("foreign object modified")
	}
	// a different autoscaler's HPA is foreign to this one
	other := autoscaler("other")
	if _, found, _ := f.Get(context.Background(), other); found {
		t.Fatal("must not find objects it does not control")
	}
}

func TestForAutoscalerOrderingIsStableForEveryParticipant(t *testing.T) {
	older, younger := autoscaler("b-older"), autoscaler("a-younger")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	younger.CreationTimestamp = metav1.Unix(2000, 0)
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(older, younger).Build()
	ctx := context.Background()
	if got, err := ForAutoscaler(ctx, c, older); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	got, err := ForAutoscaler(ctx, c, younger)
	if err != nil || len(got) != 1 || got[0].Name != "b-older" || got[0].Kind != "KubeCostAutoscaler" {
		t.Fatalf("%v %v", got, err)
	}
	if !strings.Contains(got[0].String(), "b-older") {
		t.Fatal("String()")
	}
}

func TestForAutoscalerIgnoresDeletingAndOtherTargets(t *testing.T) {
	self := autoscaler("self")
	now := metav1.Now()
	deleting := autoscaler("aaa")
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"x"}
	elsewhere := autoscaler("bbb")
	elsewhere.Spec.TargetRef.Name = "other-app"
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(self, deleting, elsewhere).Build()
	if got, err := ForAutoscaler(context.Background(), c, self); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}
