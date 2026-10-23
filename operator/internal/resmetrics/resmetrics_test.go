package resmetrics

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	fakemetrics "k8s.io/metrics/pkg/client/clientset/versioned/fake"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func pod(name string, ready bool, cpuReq, memReq string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if cpuReq != "" {
		p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if memReq != "" {
		p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

func sample(name string, age time.Duration, cpu, mem string) metricsv1beta1.PodMetrics {
	u := corev1.ResourceList{}
	if cpu != "" {
		u[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		u[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Timestamp: metav1.NewTime(now.Add(-age)),
		Containers: []metricsv1beta1.ContainerMetrics{{Name: "app", Usage: u}}}
}

var cpu = []corev1.ResourceName{corev1.ResourceCPU}

func TestComputeAveragesOverReadyPods(t *testing.T) {
	pods := []corev1.Pod{pod("a", true, "1", ""), pod("b", true, "1", ""), pod("c", false, "1", "")}
	got, err := Compute(pods, []metricsv1beta1.PodMetrics{sample("a", 10*time.Second, "800m", ""), sample("b", 10*time.Second, "400m", ""), sample("c", time.Second, "2", "")},
		cpu, now, time.Minute)
	if err != nil || got.Value != 0.6 || got.Pods != 2 {
		t.Fatalf("%+v %v (the unready pod must not count)", got, err)
	}
}

func TestComputePicksTheMostConstrainedResource(t *testing.T) {
	pods := []corev1.Pod{pod("a", true, "1", "1Gi")}
	got, err := Compute(pods, []metricsv1beta1.PodMetrics{sample("a", 0, "100m", "900Mi")},
		[]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}, now, time.Minute)
	if err != nil || got.Resource != corev1.ResourceMemory || got.Value < 0.87 || got.Value > 0.89 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestComputeRejectsStaleMissingAndPartial(t *testing.T) {
	two := []corev1.Pod{pod("a", true, "1", ""), pod("b", true, "1", "")}
	cases := map[string]struct {
		pods    []corev1.Pod
		samples []metricsv1beta1.PodMetrics
	}{
		"all stale":         {two, []metricsv1beta1.PodMetrics{sample("a", 10*time.Minute, "1", ""), sample("b", 10*time.Minute, "1", "")}},
		"future timestamps": {two, []metricsv1beta1.PodMetrics{sample("a", -time.Hour, "1", ""), sample("b", -time.Hour, "1", "")}},
		"no samples":        {two, nil},
		"below coverage":    {append(two, pod("c", true, "1", ""), pod("d", true, "1", "")), []metricsv1beta1.PodMetrics{sample("a", 0, "1", "")}},
		"no requests":       {[]corev1.Pod{pod("a", true, "", "")}, []metricsv1beta1.PodMetrics{sample("a", 0, "1", "")}},
		"no ready pods":     {[]corev1.Pod{pod("a", false, "1", "")}, []metricsv1beta1.PodMetrics{sample("a", 0, "1", "")}},
		"sample lacks cpu":  {two, []metricsv1beta1.PodMetrics{sample("a", 0, "", "1Gi"), sample("b", 0, "", "1Gi")}},
	}
	for name, tc := range cases {
		if _, err := Compute(tc.pods, tc.samples, cpu, now, time.Minute); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	// a single stale pod out of two is still above 50% coverage
	if _, err := Compute(two, []metricsv1beta1.PodMetrics{sample("a", 0, "1", ""), sample("b", 10*time.Minute, "1", "")}, cpu, now, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestClientAvailabilityDiscoveryAndCaching(t *testing.T) {
	cs := fake.NewSimpleClientset()
	calls := 0
	cs.Fake.AddReactor("*", "*", func(a clienttesting.Action) (bool, runtime.Object, error) { calls++; return false, nil, nil })
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{{Name: "pods"}, {Name: "nodes"}}}}
	c := &Client{Metrics: fakemetrics.NewSimpleClientset(), Discovery: cs.Discovery(), Now: func() time.Time { return now }}
	if err := c.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs.Resources = nil // the API disappears
	if err := c.Available(context.Background()); err != nil {
		t.Fatalf("result must be cached for the TTL: %v", err)
	}
	c.Now = func() time.Time { return now.Add(time.Minute) }
	if err := c.Available(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after the TTL the missing API must be detected: %v", err)
	}
}

func TestClientWithoutPodsResource(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{{Name: "nodes"}}}}
	c := &Client{Metrics: fakemetrics.NewSimpleClientset(), Discovery: cs.Discovery()}
	if err := c.Available(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
