// Package ownership guarantees there is a single writer of a Deployment's
// replica count.
//
// Writers considered: native HorizontalPodAutoscalers, other KubeCostAutoscalers
// and the legacy KubeCostHPA when its horizontal scaling (spec.replicas) is on.
// The operator-managed fallback HPA is the one native HPA that is allowed: it is
// owned by (controller reference) the KubeCostAutoscaler that targets the
// Deployment, and that autoscaler stops writing replicas while it exists.
package ownership

import (
	"context"
	"fmt"
	"sort"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

// Conflict describes another writer of the same Deployment's replicas.
type Conflict struct {
	Kind, Name string
	Reason     string
}

func (c Conflict) String() string { return fmt.Sprintf("%s/%s: %s", c.Kind, c.Name, c.Reason) }

func targetsDeployment(h *autoscalingv2.HorizontalPodAutoscaler, name string) bool {
	r := h.Spec.ScaleTargetRef
	return r.Kind == "Deployment" && r.Name == name && (r.APIVersion == "" || r.APIVersion == "apps/v1")
}

// ForAutoscaler returns the writers that prevent self from owning the replicas
// of deployment. An empty result means self may write.
func ForAutoscaler(ctx context.Context, c client.Reader, self *v1alpha1.KubeCostAutoscaler) ([]Conflict, error) {
	ns, dep := self.Namespace, self.Spec.TargetRef.Name
	var out []Conflict

	var hpas autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &hpas, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	for i := range hpas.Items {
		h := &hpas.Items[i]
		if !targetsDeployment(h, dep) {
			continue
		}
		if isControlledBy(h, self) {
			continue // our own fallback HPA
		}
		out = append(out, Conflict{Kind: "HorizontalPodAutoscaler", Name: h.Name, Reason: "a native HPA already scales this Deployment"})
	}

	var legacy v1alpha1.KubeCostHPAList
	if err := c.List(ctx, &legacy, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing KubeCostHPAs: %w", err)
	}
	for i := range legacy.Items {
		h := &legacy.Items[i]
		if h.Spec.TargetRef.Name == dep && h.Spec.Replicas != nil && h.DeletionTimestamp == nil {
			out = append(out, Conflict{Kind: "KubeCostHPA", Name: h.Name, Reason: "horizontal scaling (spec.replicas) is enabled for this Deployment"})
		}
	}

	var peers v1alpha1.KubeCostAutoscalerList
	if err := c.List(ctx, &peers, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing KubeCostAutoscalers: %w", err)
	}
	var contenders []*v1alpha1.KubeCostAutoscaler
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.Spec.TargetRef.Name == dep && p.DeletionTimestamp == nil {
			contenders = append(contenders, p)
		}
	}
	// Deterministic winner: oldest first, then by name. Everyone computes the same answer.
	sort.Slice(contenders, func(i, j int) bool {
		a, b := contenders[i], contenders[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	if len(contenders) > 0 && contenders[0].UID != self.UID {
		out = append(out, Conflict{Kind: "KubeCostAutoscaler", Name: contenders[0].Name, Reason: "an older KubeCostAutoscaler owns this Deployment"})
	}
	return out, nil
}

// ForLegacyHPA returns the writers that prevent a legacy KubeCostHPA from
// scaling replicas of the Deployment horizontally.
func ForLegacyHPA(ctx context.Context, c client.Reader, namespace, deployment string) ([]Conflict, error) {
	var out []Conflict
	var hpas autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &hpas, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	for i := range hpas.Items {
		if targetsDeployment(&hpas.Items[i], deployment) {
			out = append(out, Conflict{Kind: "HorizontalPodAutoscaler", Name: hpas.Items[i].Name, Reason: "a native HPA already scales this Deployment"})
		}
	}
	var peers v1alpha1.KubeCostAutoscalerList
	if err := c.List(ctx, &peers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing KubeCostAutoscalers: %w", err)
	}
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.Spec.TargetRef.Name == deployment && p.DeletionTimestamp == nil {
			out = append(out, Conflict{Kind: "KubeCostAutoscaler", Name: p.Name, Reason: "owns the replicas of this Deployment"})
		}
	}
	return out, nil
}

func isControlledBy(h *autoscalingv2.HorizontalPodAutoscaler, owner *v1alpha1.KubeCostAutoscaler) bool {
	for _, r := range h.OwnerReferences {
		if r.Controller != nil && *r.Controller && r.UID == owner.UID && r.Kind == "KubeCostAutoscaler" {
			return true
		}
	}
	return false
}
