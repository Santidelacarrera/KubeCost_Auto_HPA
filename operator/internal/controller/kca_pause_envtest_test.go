//go:build envtest

package controller

// Emergency pause against a REAL kube-apiserver. While the annotation is set the
// controller must not write replicas, must not revert changes made by others, and
// resuming must follow current demand instead of restoring pre-pause values.
//
// The paused phase uses LOW demand on purpose: without the pause, the controller
// would scale down once the 10-minute downscale window has passed, so an unchanged
// replica count is evidence of the pause and not of a hold decision.
// Cost and resource metrics are fakes; the API server, the scale subresource and
// the compare-and-swap writes are real.

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

// syncPodsToReplicas plays the part of the ReplicaSet controller, which envtest does
// not run: it creates pods up to spec.replicas and marks every pod Ready through the
// status subresource. Without it the controller correctly holds on ReplicasNotReady.
func syncPodsToReplicas(t *testing.T, e *realEnv) {
	t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	mustNoErr(t, e.c.Get(ctx, types.NamespacedName{Namespace: "production", Name: "payments-api"}, &dep))
	var pods corev1.PodList
	mustNoErr(t, e.c.List(ctx, &pods, client.InNamespace("production"), client.MatchingLabels(dep.Spec.Selector.MatchLabels)))
	for i := len(pods.Items); i < int(*dep.Spec.Replicas); i++ {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("payments-api-sim-%d", i), Namespace: "production", Labels: dep.Spec.Selector.MatchLabels},
			Spec:       corev1.PodSpec{NodeName: "od-1", Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}
		mustNoErr(t, e.c.Create(ctx, p))
		pods.Items = append(pods.Items, *p)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		mustNoErr(t, e.c.Status().Update(ctx, p))
	}
	// The controller reads Deployment.status.readyReplicas, which the same component maintains.
	n := *dep.Spec.Replicas
	dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas = n, n, n, n
	mustNoErr(t, e.c.Status().Update(ctx, &dep))
}

func setPauseAnnotation(t *testing.T, e *realEnv, on bool) {
	t.Helper()
	ctx := context.Background()
	var a v1alpha1.KubeCostAutoscaler
	mustNoErr(t, e.c.Get(ctx, kcaReq.NamespacedName, &a))
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	if on {
		a.Annotations[v1alpha1.AnnotationPause] = "true"
	} else {
		delete(a.Annotations, v1alpha1.AnnotationPause)
	}
	mustNoErr(t, e.c.Update(ctx, &a))
}

// setReplicasExternally simulates `kubectl scale` or another controller.
func setReplicasExternally(t *testing.T, e *realEnv, n int32) {
	t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	mustNoErr(t, e.c.Get(ctx, types.NamespacedName{Namespace: "production", Name: "payments-api"}, &dep))
	dep.Spec.Replicas = &n
	mustNoErr(t, e.c.Update(ctx, &dep))
}

// evaluationInterval is the CRD default. Larger steps look like controller silence to the
// stabilisation history (MaxGap), which restarts warm-up and would hide the pause.
const evaluationInterval = 30 * time.Second

func TestRealAPIServer_PauseWritesNothingAndResumeRevertsNothing(t *testing.T) {
	e := newRealEnv(t, nil, 4) // demand-only

	// 1. Normal operation: a 97% utilisation scales up before the pause.
	e.res.util = 0.97
	syncPodsToReplicas(t, e)
	e.reconcile()
	beforePause := e.replicas()
	if beforePause <= 4 {
		t.Fatalf("setup: expected a scale-up before pausing, replicas=%d", beforePause)
	}

	// 2. Paused, with low demand that would otherwise scale down past the window.
	setPauseAnnotation(t, e, true)
	for i := 0; i < 40; i++ { // 20 minutes at the default evaluation interval
		e.now = e.now.Add(evaluationInterval)
		e.res.util = 0.2
		syncPodsToReplicas(t, e)
		e.reconcile()
	}
	if got := e.replicas(); got != beforePause {
		t.Fatalf("paused controller changed replicas: %d -> %d", beforePause, got)
	}
	if c := meta.FindStatusCondition(e.status().Conditions, v1alpha1.ConditionScalingActive); c == nil || c.Reason != "Paused" {
		t.Fatalf("pause is not reported in status: %+v", c)
	}

	// 3. Someone else changes the replica count while paused. It must not be reverted.
	external := beforePause + 3
	setReplicasExternally(t, e, external)
	for i := 0; i < 40; i++ { // 20 minutes at the default evaluation interval
		e.now = e.now.Add(evaluationInterval)
		e.res.util = 0.2
		syncPodsToReplicas(t, e)
		e.reconcile()
	}
	if got := e.replicas(); got != external {
		t.Fatalf("paused controller reverted an external change: want %d, got %d", external, got)
	}

	// 4. Resume. The controller follows current demand (low here) but must not
	// restore pre-pause values or jump above the externally set count.
	setPauseAnnotation(t, e, false)
	e.now = e.now.Add(evaluationInterval)
	e.res.util = 0.2
	syncPodsToReplicas(t, e)
	e.reconcile()
	if got := e.replicas(); got > external || got < 2 {
		t.Fatalf("resume must follow demand within [2,%d], got %d", external, got)
	}
	if c := meta.FindStatusCondition(e.status().Conditions, v1alpha1.ConditionScalingActive); c != nil && c.Reason == "Paused" {
		t.Fatal("status still reports paused after the annotation was removed")
	}
}
