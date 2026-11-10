package main

// Fake clients: argument and object-shape handling only; the real-cluster run is hack/lab-applier.sh.

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func recFor() recommend.Recommendation {
	return recommend.Recommendation{ID: "Rec_1", Namespace: "shop", Workload: "Api", Kind: "Deployment", Container: "app", Confidence: recommend.ConfMed,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: 2, Recommended: 1, Limit: 4, PotentialMonthlyUSD: 10},
		Memory: recommend.ResourceResult{Action: recommend.NoChange, Current: 1 << 30, Recommended: 1 << 30}}
}

func newDyn() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{wrecGVR: "WorkloadRecommendationList"})
}

func TestNameIsStableDNSSafeAndShort(t *testing.T) {
	r := recFor()
	if n := wrecName(r); n != "api-rec-1" {
		t.Fatal(n)
	}
	r.Workload = strings.Repeat("x", 100)
	if n := wrecName(r); len(n) > 63 || strings.HasSuffix(n, "-") {
		t.Fatal(n)
	}
}

func TestPublishCreatesOneObjectAndIsIdempotent(t *testing.T) {
	dyn := newDyn()
	name, err := publishRecommendation(context.Background(), dyn, recFor(), "latency")
	if err != nil || name != "api-rec-1" {
		t.Fatalf("%v %q", err, name)
	}
	obj, err := dyn.Resource(wrecGVR).Namespace("shop").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := unstructured.NestedString(obj.Object, "spec", "recommendationID")
	slo, _, _ := unstructured.NestedString(obj.Object, "spec", "sloQueryName")
	cpu, _, _ := unstructured.NestedString(obj.Object, "spec", "summary", "cpu")
	rec, _, _ := unstructured.NestedString(obj.Object, "spec", "record")
	if id != "Rec_1" || slo != "latency" || !strings.Contains(cpu, "2000m -> 1000m") || !strings.Contains(rec, `"Rec_1"`) {
		t.Fatalf("%v", obj.Object["spec"])
	}
	if _, err := publishRecommendation(context.Background(), dyn, recFor(), "latency"); err != nil {
		t.Fatalf("publishing the same recommendation twice must be a no-op: %v", err)
	}
	r := recFor()
	r.Abstained = true
	if _, err := publishRecommendation(context.Background(), dyn, r, ""); err == nil {
		t.Fatal("an abstention was published")
	}
	r = recFor()
	r.CPU.Action = recommend.NoChange
	if _, err := publishRecommendation(context.Background(), dyn, r, ""); err == nil {
		t.Fatal("a no-op was published")
	}
}

func TestApproveWritesTheStatusWithTheServerIdentity(t *testing.T) {
	two := int32(2)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "Api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}}}}}
	kc := k8sfake.NewSimpleClientset(d)
	kc.PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectReview{Status: authv1.SelfSubjectReviewStatus{UserInfo: authv1.UserInfo{Username: "alice@example.com"}}}, nil
	})
	dyn := newDyn()
	if _, err := publishRecommendation(context.Background(), dyn, recFor(), ""); err != nil {
		t.Fatal(err)
	}
	ex := &execute.Executor{KC: kc, Dyn: dyn}
	who, err := approveRecommendation(context.Background(), ex, recFor())
	if err != nil || who != "alice@example.com" {
		t.Fatalf("%v %q", err, who)
	}
	obj, _ := dyn.Resource(wrecGVR).Namespace("shop").Get(context.Background(), "api-rec-1", metav1.GetOptions{})
	by, _, _ := unstructured.NestedString(obj.Object, "status", "approval", "by")
	rid, _, _ := unstructured.NestedString(obj.Object, "status", "approval", "recommendationID")
	if by != "alice@example.com" || rid != "Rec_1" {
		t.Fatalf("%v", obj.Object["status"])
	}
	// a stale recommendation is not approved
	stale := recFor()
	stale.CPU.Current = 9
	if _, err := approveRecommendation(context.Background(), ex, stale); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("%v", err)
	}
	// nothing published: nothing to approve
	if _, err := approveRecommendation(context.Background(), &execute.Executor{KC: kc, Dyn: newDyn()}, recFor()); err == nil {
		t.Fatal("approved a recommendation that was never published")
	}
}
