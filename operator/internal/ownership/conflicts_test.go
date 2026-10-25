package ownership

import (
	"context"
	"errors"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

func nativeHPA(name, kind, apiVersion, target string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, APIVersion: apiVersion, Name: target},
			MaxReplicas:    5,
		},
	}
}

func legacyHPA(name string, horizontal bool) *v1alpha1.KubeCostHPA {
	h := &v1alpha1.KubeCostHPA{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:       v1alpha1.KubeCostHPASpec{TargetRef: v1alpha1.TargetRef{Name: "api"}},
	}
	if horizontal {
		h.Spec.Replicas = &v1alpha1.ReplicaSpec{Min: 1, Max: 4}
	}
	return h
}

func names(cs []Conflict) map[string]string {
	out := map[string]string{}
	for _, c := range cs {
		out[c.Kind+"/"+c.Name] = c.Reason
	}
	return out
}

func TestForAutoscalerSeesEveryOtherReplicaWriter(t *testing.T) {
	self := autoscaler("self")
	own := nativeHPA("kca-self", "Deployment", "apps/v1", "api")
	yes := true
	own.OwnerReferences = []metav1.OwnerReference{{Kind: "KubeCostAutoscaler", Name: "self", UID: self.UID, Controller: &yes}}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
		self,
		own, // our own fallback HPA is not a conflict
		nativeHPA("web-hpa", "Deployment", "apps/v1", "api"),
		nativeHPA("legacy-version", "Deployment", "", "api"), // empty apiVersion still means apps/v1
		nativeHPA("other-app", "Deployment", "apps/v1", "other"),
		nativeHPA("statefulset", "StatefulSet", "apps/v1", "api"),
		nativeHPA("other-group", "Deployment", "example.com/v1", "api"),
		legacyHPA("horizontal", true),
		legacyHPA("vertical-only", false),
	).Build()

	got, err := ForAutoscaler(context.Background(), c, self)
	if err != nil {
		t.Fatal(err)
	}
	n := names(got)
	want := []string{"HorizontalPodAutoscaler/web-hpa", "HorizontalPodAutoscaler/legacy-version", "KubeCostHPA/horizontal"}
	if len(n) != len(want) {
		t.Fatalf("conflicts = %v, want exactly %v", n, want)
	}
	for _, k := range want {
		if _, ok := n[k]; !ok {
			t.Errorf("missing conflict %s in %v", k, n)
		}
	}
}

func TestForAutoscalerIgnoresDeletingLegacyHPA(t *testing.T) {
	self := autoscaler("self")
	now := metav1.Now()
	l := legacyHPA("going-away", true)
	l.DeletionTimestamp, l.Finalizers = &now, []string{"x"}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(self, l).Build()
	if got, err := ForAutoscaler(context.Background(), c, self); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestForLegacyHPA(t *testing.T) {
	now := metav1.Now()
	deleting := autoscaler("deleting")
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"x"}
	elsewhere := autoscaler("elsewhere")
	elsewhere.Spec.TargetRef.Name = "other"
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
		nativeHPA("web-hpa", "Deployment", "apps/v1", "api"),
		nativeHPA("other-app", "Deployment", "apps/v1", "other"),
		autoscaler("cost"),
		deleting,
		elsewhere,
	).Build()

	got, err := ForLegacyHPA(context.Background(), c, "prod", "api")
	if err != nil {
		t.Fatal(err)
	}
	n := names(got)
	if len(n) != 2 || n["HorizontalPodAutoscaler/web-hpa"] == "" || n["KubeCostAutoscaler/cost"] == "" {
		t.Fatalf("conflicts = %v", n)
	}
	if got, err := ForLegacyHPA(context.Background(), c, "prod", "free"); err != nil || len(got) != 0 {
		t.Fatalf("free deployment: %v %v", got, err)
	}
	if got, err := ForLegacyHPA(context.Background(), c, "other-ns", "api"); err != nil || len(got) != 0 {
		t.Fatalf("other namespace must not leak: %v %v", got, err)
	}
}

// A failed read must surface as an error: treating "could not list" as "no conflict"
// would let two writers fight over the replica count.
func TestReaderErrorsAreNeverReadAsNoConflict(t *testing.T) {
	boom := errors.New("boom")
	for _, failing := range []string{"HorizontalPodAutoscalerList", "KubeCostHPAList", "KubeCostAutoscalerList"} {
		failing := failing
		c := fake.NewClientBuilder().WithScheme(scheme()).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if gvk, _, _ := scheme().ObjectKinds(list); len(gvk) > 0 && gvk[0].Kind == failing {
					return boom
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
		if _, err := ForAutoscaler(context.Background(), c, autoscaler("self")); !errors.Is(err, boom) {
			t.Errorf("ForAutoscaler with failing %s: err = %v", failing, err)
		}
		if failing == "KubeCostHPAList" {
			continue // ForLegacyHPA does not read KubeCostHPAs
		}
		if _, err := ForLegacyHPA(context.Background(), c, "prod", "api"); !errors.Is(err, boom) {
			t.Errorf("ForLegacyHPA with failing %s: err = %v", failing, err)
		}
	}
}
