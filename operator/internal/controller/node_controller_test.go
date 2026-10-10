package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
)

func node(name string, spot, itn bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if spot {
		n.Labels["karpenter.sh/capacity-type"] = "spot"
	}
	if itn {
		n.Spec.Taints = []corev1.Taint{{Key: "aws-node-termination-handler/spot-itn", Effect: corev1.TaintEffectNoSchedule}}
	}
	return n
}

func nodeEnv(objs ...*corev1.Node) (*NodeReconciler, *fake.Clientset, *[]string) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	var cobjs []client.Object
	var kobjs []runtime.Object
	for _, o := range objs {
		cobjs = append(cobjs, o)
		kobjs = append(kobjs, o.DeepCopy())
	}
	pod := newPod("api-1")
	pod.Spec.NodeName = "spot-1"
	t := true
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &t}}
	kobjs = append(kobjs, pod)
	cs := fake.NewSimpleClientset(kobjs...)
	evicted := &[]string{}
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(clienttesting.CreateAction).GetObject().(*policyv1.Eviction)
		*evicted = append(*evicted, ev.Name)
		_ = cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name)
		return true, nil, nil
	})
	c := crfake.NewClientBuilder().WithScheme(s).WithObjects(cobjs...).Build()
	return &NodeReconciler{Client: c, Drainer: &actuator.Drainer{Client: cs, PollInterval: time.Millisecond, Timeout: time.Second}}, cs, evicted
}

func TestInterruptedSpotNodeIsDrainedOnce(t *testing.T) {
	r, cs, evicted := nodeEnv(node("spot-1", true, true), node("od-1", false, false))
	rq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "spot-1"}}
	if _, err := r.Reconcile(context.Background(), rq); err != nil {
		t.Fatal(err)
	}
	if len(*evicted) != 1 {
		t.Fatalf("evicted %v", *evicted)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "spot-1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("not cordoned")
	}
	var cn corev1.Node
	_ = r.Get(context.Background(), rq.NamespacedName, &cn)
	if cn.Annotations[DrainedAnnotation] != "true" {
		t.Fatal("not marked drained")
	}
	r.Reconcile(context.Background(), rq)
	if len(*evicted) != 1 {
		t.Fatal("drained twice")
	}
}

func TestIgnoresHealthyAndOnDemandNodes(t *testing.T) {
	for _, n := range []*corev1.Node{node("spot-1", true, false), node("spot-1", false, true)} {
		r, _, evicted := nodeEnv(n, node("od-1", false, false))
		r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "spot-1"}})
		if len(*evicted) != 0 {
			t.Fatalf("drained %+v", n.Labels)
		}
	}
}

func TestDrainFailureRequeues(t *testing.T) {
	r, _, _ := nodeEnv(node("spot-1", true, true)) // no other node: refuses
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "spot-1"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
