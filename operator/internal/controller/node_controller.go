package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
)

// DrainedAnnotation marks a node already handled, keeping reconciles idempotent.
const DrainedAnnotation = "autoscaling.kubecost.io/spot-drained"

// InterruptionAnnotation can be set by any external signal source to request a drain.
const InterruptionAnnotation = "autoscaling.kubecost.io/spot-interruption"

var spotLabels = map[string][]string{
	"eks.amazonaws.com/capacityType":    {"SPOT"},
	"karpenter.sh/capacity-type":        {"spot"},
	"cloud.google.com/gke-spot":         {"true"},
	"cloud.google.com/gke-provisioning": {"spot"},
}

var interruptionTaints = map[string]bool{
	"aws-node-termination-handler/spot-itn":                 true,
	"aws-node-termination-handler/rebalance-recommendation": true,
	"cloud.google.com/impending-node-termination":           true,
}

// IsSpotNode reports whether the node runs on interruptible capacity.
func IsSpotNode(n *corev1.Node) bool {
	for k, vals := range spotLabels {
		for _, v := range vals {
			if n.Labels[k] == v {
				return true
			}
		}
	}
	return false
}

// IsInterrupted reports whether a Spot node has a pending interruption/rebalance signal.
func IsInterrupted(n *corev1.Node) bool {
	if n.Annotations[InterruptionAnnotation] == "true" {
		return true
	}
	for _, t := range n.Spec.Taints {
		if interruptionTaints[t.Key] {
			return true
		}
	}
	return false
}

// NodeReconciler cordons and drains Spot nodes that are about to be reclaimed,
// so workloads move to On-Demand capacity before the node disappears.
type NodeReconciler struct {
	client.Client
	Drainer  *actuator.Drainer
	Recorder record.EventRecorder
	// RetryAfter is the requeue delay after a failed drain.
	RetryAfter time.Duration
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !IsSpotNode(&node) || !IsInterrupted(&node) || node.Annotations[DrainedAnnotation] == "true" {
		return ctrl.Result{}, nil
	}
	lg := log.FromContext(ctx)
	lg.Info("draining interrupted spot node", "node", node.Name)
	if err := r.Drainer.Drain(ctx, node.Name); err != nil {
		cResultDrain("failed")
		lg.Error(err, "drain failed", "node", node.Name)
		if r.Recorder != nil {
			r.Recorder.Event(&node, corev1.EventTypeWarning, "SpotDrainFailed", err.Error())
		}
		retry := r.RetryAfter
		if retry <= 0 {
			retry = 15 * time.Second
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[DrainedAnnotation] = "true"
	if err := r.Patch(ctx, &node, patch); err != nil {
		return ctrl.Result{}, err
	}
	cResultDrain("ok")
	if r.Recorder != nil {
		r.Recorder.Event(&node, corev1.EventTypeNormal, "SpotDrained", "node cordoned and drained ahead of interruption")
	}
	return ctrl.Result{}, nil
}

func cResultDrain(result string) { cDrains.WithLabelValues(result).Inc() }

// SetupWithManager registers the controller.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&corev1.Node{}).Named("spot-node").Complete(r)
}
