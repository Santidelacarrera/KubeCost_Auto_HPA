package actuator

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func pod(name, node string, cpu, limit string, owner string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if limit != "" {
		p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)}
	}
	if owner != "" {
		t := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "o", Controller: &t}}
	}
	return p
}

func TestResizeUsesResizeSubresourceAndKeepsRatio(t *testing.T) {
	p := pod("p1", "n1", "1", "2", "ReplicaSet")
	cs := fake.NewSimpleClientset(p)
	var sub string
	cs.PrependReactor("patch", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sub = a.GetSubresource()
		return false, nil, nil
	})
	changed, err := (&Resizer{cs}).ResizeCPU(context.Background(), p, "app", 500)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if sub != "resize" {
		t.Fatalf("subresource = %q", sub)
	}
	got, _ := cs.CoreV1().Pods("shop").Get(context.Background(), "p1", metav1.GetOptions{})
	c := got.Spec.Containers[0].Resources
	if c.Requests.Cpu().MilliValue() != 500 || c.Limits.Cpu().MilliValue() != 1000 {
		t.Fatalf("requests=%v limits=%v", c.Requests.Cpu(), c.Limits.Cpu())
	}
}

func TestResizeNoopRefusalsAndInflight(t *testing.T) {
	r := &Resizer{fake.NewSimpleClientset()}
	ctx := context.Background()
	p := pod("p", "n", "500m", "", "")
	if ch, err := r.ResizeCPU(ctx, p, "app", 500); ch || err != nil {
		t.Fatalf("same value should be no-op: %v %v", ch, err)
	}
	if _, err := r.ResizeCPU(ctx, p, "nope", 100); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("got %v", err)
	}
	p.Spec.Containers[0].ResizePolicy = []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.RestartContainer}}
	if _, err := r.ResizeCPU(ctx, p, "app", 100); !errors.Is(err, ErrWouldRestart) {
		t.Fatalf("got %v", err)
	}
	p.Spec.Containers[0].ResizePolicy = nil
	p.Status.Conditions = []corev1.PodCondition{{Type: podResizeInProgress, Status: corev1.ConditionTrue}}
	if _, err := r.ResizeCPU(ctx, p, "app", 100); !errors.Is(err, ErrResizeInFlight) {
		t.Fatalf("got %v", err)
	}
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}

// evictReactor deletes the pod on eviction, optionally rejecting the first N attempts with 429 (PDB).
func evictReactor(cs *fake.Clientset, rejectFirst int, evicted *[]string) {
	attempts := 0
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(clienttesting.CreateAction).GetObject().(*policyv1.Eviction)
		attempts++
		if attempts <= rejectFirst {
			return true, nil, apierrors.NewTooManyRequests("pdb", 1)
		}
		*evicted = append(*evicted, ev.Name)
		_ = cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name)
		return true, nil, nil
	})
}

func TestDrainEvictsManagedPodsSkipsDaemonSets(t *testing.T) {
	cs := fake.NewSimpleClientset(readyNode("spot-1"), readyNode("od-1"),
		pod("web", "spot-1", "1", "", "ReplicaSet"), pod("agent", "spot-1", "1", "", "DaemonSet"), pod("other", "od-1", "1", "", "ReplicaSet"))
	var evicted []string
	evictReactor(cs, 0, &evicted)
	d := &Drainer{Client: cs, PollInterval: time.Millisecond, Timeout: time.Second}
	if err := d.Drain(context.Background(), "spot-1"); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != "web" {
		t.Fatalf("evicted %v", evicted)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "spot-1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}
}

func TestDrainRetriesWhenPDBBlocks(t *testing.T) {
	cs := fake.NewSimpleClientset(readyNode("spot-1"), readyNode("od-1"), pod("web", "spot-1", "1", "", "ReplicaSet"))
	var evicted []string
	evictReactor(cs, 2, &evicted)
	d := &Drainer{Client: cs, PollInterval: time.Millisecond, Timeout: time.Second}
	if err := d.Drain(context.Background(), "spot-1"); err != nil || len(evicted) != 1 {
		t.Fatalf("err=%v evicted=%v", err, evicted)
	}
}

func TestDrainTimesOutOnPersistentPDB(t *testing.T) {
	cs := fake.NewSimpleClientset(readyNode("spot-1"), readyNode("od-1"), pod("web", "spot-1", "1", "", "ReplicaSet"))
	var evicted []string
	evictReactor(cs, 1<<30, &evicted)
	d := &Drainer{Client: cs, PollInterval: time.Millisecond, Timeout: 30 * time.Millisecond}
	if err := d.Drain(context.Background(), "spot-1"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDrainRefusesWithoutCapacityOrWithUnmanagedPods(t *testing.T) {
	cs := fake.NewSimpleClientset(readyNode("spot-1"), pod("web", "spot-1", "1", "", "ReplicaSet"))
	d := &Drainer{Client: cs, PollInterval: time.Millisecond}
	if err := d.Drain(context.Background(), "spot-1"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("got %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "spot-1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("must not cordon the last schedulable node")
	}

	cs = fake.NewSimpleClientset(readyNode("spot-1"), readyNode("od-1"), pod("bare", "spot-1", "1", "", ""))
	d = &Drainer{Client: cs, PollInterval: time.Millisecond}
	if err := d.Drain(context.Background(), "spot-1"); err == nil {
		t.Fatal("expected unmanaged pod blocker")
	}
}

func TestUncordon(t *testing.T) {
	n := readyNode("n")
	n.Spec.Unschedulable = true
	cs := fake.NewSimpleClientset(n)
	d := &Drainer{Client: cs}
	if err := d.Uncordon(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.CoreV1().Nodes().Get(context.Background(), "n", metav1.GetOptions{})
	if got.Spec.Unschedulable {
		t.Fatal("still cordoned")
	}
}

func TestResizeMemoryPreservesLimitRatioAndRefusesRestart(t *testing.T) {
	p := pod("p1", "n1", "1", "2", "ReplicaSet")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Mi")
	p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("200Mi")
	cs := fake.NewSimpleClientset(p)
	r := &Resizer{cs}
	if ch, err := r.Resize(context.Background(), p, "app", Target{MemoryBytes: 150 << 20}); err != nil || !ch {
		t.Fatalf("ch=%v err=%v", ch, err)
	}
	got, _ := cs.CoreV1().Pods("shop").Get(context.Background(), "p1", metav1.GetOptions{})
	rs := got.Spec.Containers[0].Resources
	if rs.Requests.Memory().Value() != 150<<20 || rs.Limits.Memory().Value() != 300<<20 {
		t.Fatalf("mem requests=%v limits=%v", rs.Requests.Memory(), rs.Limits.Memory())
	}
	if rs.Requests.Cpu().MilliValue() != 1000 {
		t.Fatal("cpu must be untouched when CPUMillis is zero")
	}
	p.Spec.Containers[0].ResizePolicy = []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer}}
	if _, err := r.Resize(context.Background(), p, "app", Target{MemoryBytes: 1 << 30}); !errors.Is(err, ErrWouldRestart) {
		t.Fatalf("got %v", err)
	}
}

func TestSetReplicas(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}}
	cs := fake.NewSimpleClientset(d)
	fakeScale(cs)
	r := &Resizer{cs}
	if err := r.SetReplicas(context.Background(), "shop", "api", 4); err != nil {
		t.Fatal(err)
	}
	sc, _ := cs.AppsV1().Deployments("shop").GetScale(context.Background(), "api", metav1.GetOptions{})
	if sc.Spec.Replicas != 4 {
		t.Fatalf("replicas=%d", sc.Spec.Replicas)
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
