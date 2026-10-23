package actuator

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrReplicaDrift means someone else changed the replica count since it was
// observed; the write is refused so a stale decision never overwrites it.
var ErrReplicaDrift = errors.New("replica count changed since it was observed")

// ReplicaScaler changes a Deployment's replica count.
type ReplicaScaler interface {
	// Scale sets replicas to `to` only if the current value is `from`.
	Scale(ctx context.Context, namespace, name string, from, to int32) error
}

// DeploymentScaler writes through the scale subresource with compare-and-swap
// semantics (value check plus the Scale object's resourceVersion).
type DeploymentScaler struct{ Client kubernetes.Interface }

func (s *DeploymentScaler) Scale(ctx context.Context, namespace, name string, from, to int32) error {
	deployments := s.Client.AppsV1().Deployments(namespace)
	sc, err := deployments.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get scale %s/%s: %w", namespace, name, err)
	}
	if sc.Spec.Replicas != from {
		return fmt.Errorf("%w: expected %d, found %d", ErrReplicaDrift, from, sc.Spec.Replicas)
	}
	sc.Spec.Replicas = to
	if _, err := deployments.UpdateScale(ctx, name, sc, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update scale %s/%s: %w", namespace, name, err)
	}
	return nil
}
