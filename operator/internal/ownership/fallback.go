package ownership

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

// ErrNotOurs is returned when an object with the fallback name exists but is
// not controlled by the autoscaler: it is never modified or deleted.
var ErrNotOurs = errors.New("an object with the fallback HPA name exists and is not owned by this autoscaler")

// ManagedByLabel marks fallback HPAs created by this operator.
const ManagedByLabel = "autoscaling.kubecost.io/managed-by"

// Fallback manages the native HPA used while KubeCost data is unavailable.
type Fallback struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// HPAName is deterministic and within the 253-char limit.
func HPAName(autoscaler string) string {
	n := "kca-" + autoscaler
	if len(n) <= 63 {
		return n
	}
	h := sha256.Sum256([]byte(autoscaler))
	return n[:50] + "-" + hex.EncodeToString(h[:6])
}

// Ensure creates or updates the fallback HPA. The HPA is controlled by the
// autoscaler (garbage-collected with it). It scales on CPU utilisation within
// the autoscaler's replica bounds.
func (f *Fallback) Ensure(ctx context.Context, owner *v1alpha1.KubeCostAutoscaler, cpuPercent int32) (created bool, err error) {
	desired := f.desired(owner, cpuPercent)
	var cur autoscalingv2.HorizontalPodAutoscaler
	err = f.Client.Get(ctx, client.ObjectKeyFromObject(desired), &cur)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(owner, desired, f.Scheme); err != nil {
			return false, err
		}
		if err := f.Client.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("creating fallback HPA: %w", err)
		}
		return true, nil
	case err != nil:
		return false, err
	}
	if !isControlledBy(&cur, owner) {
		return false, ErrNotOurs
	}
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec = desired.Spec
	if err := f.Client.Patch(ctx, &cur, patch); err != nil {
		return false, fmt.Errorf("updating fallback HPA: %w", err)
	}
	return false, nil
}

// Get returns the fallback HPA when it exists and is controlled by the autoscaler.
func (f *Fallback) Get(ctx context.Context, owner *v1alpha1.KubeCostAutoscaler) (*autoscalingv2.HorizontalPodAutoscaler, bool, error) {
	var cur autoscalingv2.HorizontalPodAutoscaler
	key := client.ObjectKey{Namespace: owner.Namespace, Name: HPAName(owner.Name)}
	if err := f.Client.Get(ctx, key, &cur); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if !isControlledBy(&cur, owner) {
		return nil, false, nil
	}
	return &cur, true, nil
}

// Remove deletes the fallback HPA if (and only if) the autoscaler controls it.
func (f *Fallback) Remove(ctx context.Context, owner *v1alpha1.KubeCostAutoscaler) (removed bool, err error) {
	var cur autoscalingv2.HorizontalPodAutoscaler
	key := client.ObjectKey{Namespace: owner.Namespace, Name: HPAName(owner.Name)}
	if err := f.Client.Get(ctx, key, &cur); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !isControlledBy(&cur, owner) {
		return false, nil
	}
	if err := f.Client.Delete(ctx, &cur); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (f *Fallback) desired(owner *v1alpha1.KubeCostAutoscaler, cpuPercent int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: owner.Namespace,
			Name:      HPAName(owner.Name),
			Labels:    map[string]string{ManagedByLabel: "kubecost-auto-hpa"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: owner.Spec.TargetRef.Name},
			MinReplicas:    ptr.To(owner.Spec.MinReplicas),
			MaxReplicas:    owner.Spec.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(cpuPercent),
					},
				},
			}},
		},
	}
}
