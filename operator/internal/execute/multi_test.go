package execute

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func depWithSidecar() *corev1.Container {
	return &corev1.Container{Name: "proxy", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}}}
}

func multiRec(sidecarCurrentCPU float64) recommend.Recommendation {
	r := rec()
	r.Additional = []recommend.ContainerResult{{Container: "proxy", Confidence: recommend.ConfHigh,
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: sidecarCurrentCPU, Recommended: 0.25, Limit: 1},
		Memory: recommend.ResourceResult{Action: recommend.NoChange, Current: 256 << 20, Recommended: 256 << 20}}}
	return r
}

// A combined record changes the app AND its sidecar in ONE update (one rollout), records both in the previous state so a rollback
// restores the whole pod, and names both containers in the audit.
func TestMultiContainerRecordIsAppliedInOneUpdate(t *testing.T) {
	d := dep()
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, *depWithSidecar())
	e := newEnv(d)
	updates := 0
	e.kc.PrependReactor("update", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	res, err := e.ex.Apply(context.Background(), multiRec(0.5), pol(), true)
	if err != nil || !res.Applied {
		t.Fatalf("%v %+v", err, res.Decision)
	}
	if updates != 1 {
		t.Fatalf("one rollout means one update, got %d", updates)
	}
	got, _ := e.kc.AppsV1().Deployments("shop").Get(context.Background(), "api", metav1.GetOptions{})
	cs := got.Spec.Template.Spec.Containers
	if cs[0].Resources.Requests.Cpu().String() != "1" || cs[1].Resources.Requests.Cpu().String() != "250m" || cs[1].Resources.Requests.Memory().String() != "256Mi" {
		t.Fatalf("app %v, proxy %v", cs[0].Resources.Requests, cs[1].Resources.Requests)
	}
	if _, ok := res.Previous.Containers["proxy"]; !ok {
		t.Fatalf("the sidecar must be in the previous state: %+v", res.Previous)
	}
	var applied bool
	for _, ev := range e.aud.Events {
		if ev.Type == "applied" {
			names, _ := ev.Details["containers"].([]string)
			applied = len(names) == 2
		}
	}
	if !applied {
		t.Fatal("the applied event must name both containers")
	}
}

// If ANY container of the record is stale or missing, the whole record is refused and nothing is written.
func TestMultiContainerRecordIsRefusedWholeWhenOneContainerIsStaleOrMissing(t *testing.T) {
	d := dep()
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, *depWithSidecar())
	e := newEnv(d)
	res, err := e.ex.Apply(context.Background(), multiRec(0.8), pol(), true)
	if err != nil || res.Applied || res.Decision.Allowed || !strings.Contains(strings.Join(res.Decision.Reasons, " "), "container proxy: stale") {
		t.Fatalf("%v %+v", err, res.Decision)
	}
	if c := e.container(t); c.Resources.Requests.Cpu().String() != "2" {
		t.Fatalf("nothing may change: %v", c.Resources.Requests)
	}
	e2 := newEnv(dep()) // no sidecar at all
	res, _ = e2.ex.Apply(context.Background(), multiRec(0.5), pol(), true)
	if res.Applied || !strings.Contains(strings.Join(res.Decision.Reasons, " "), `container "proxy" not found`) {
		t.Fatalf("%+v", res.Decision)
	}
	if err := e2.ex.Fresh(context.Background(), multiRec(0.5)); err == nil {
		t.Fatal("Fresh must check every container")
	}
}
