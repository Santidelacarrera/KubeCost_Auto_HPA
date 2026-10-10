package actuator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ErrNoCapacity is returned when draining would leave no schedulable node.
var ErrNoCapacity = errors.New("no other schedulable node available")

// Drainer cordons and drains nodes using the Eviction API, so PodDisruptionBudgets
// are always honoured.
type Drainer struct {
	Client kubernetes.Interface
	// Timeout bounds the whole drain.
	Timeout time.Duration
	// PollInterval is the wait between eviction retries / deletion checks.
	PollInterval time.Duration
	// GracePeriodSeconds overrides the pod grace period when non-nil.
	GracePeriodSeconds *int64
}

func (d *Drainer) poll() time.Duration {
	if d.PollInterval <= 0 {
		return 2 * time.Second
	}
	return d.PollInterval
}

// Cordon marks the node unschedulable (idempotent).
func (d *Drainer) Cordon(ctx context.Context, node string) error {
	_, err := d.Client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
	return err
}

// Uncordon makes the node schedulable again.
func (d *Drainer) Uncordon(ctx context.Context, node string) error {
	_, err := d.Client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType,
		[]byte(`{"spec":{"unschedulable":false}}`), metav1.PatchOptions{})
	return err
}

// CheckCapacity verifies that at least one other Ready, schedulable node
// exists to receive the evicted pods.
func (d *Drainer) CheckCapacity(ctx context.Context, node string) error {
	nodes, err := d.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range nodes.Items {
		if n.Name == node || n.Spec.Unschedulable {
			continue
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return nil
			}
		}
	}
	return ErrNoCapacity
}

// evictable splits pods on the node into those to evict and those that make a
// safe drain impossible (unmanaged pods).
func evictable(pods []corev1.Pod) (evict []corev1.Pod, blockers []string) {
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
			continue
		}
		owner := metav1.GetControllerOf(&p)
		if owner != nil && owner.Kind == "DaemonSet" {
			continue
		}
		if owner == nil {
			blockers = append(blockers, p.Namespace+"/"+p.Name)
			continue
		}
		evict = append(evict, p)
	}
	return
}

// Drain cordons the node (after a capacity check) and evicts its pods,
// retrying PDB-blocked evictions until the timeout.
func (d *Drainer) Drain(ctx context.Context, node string) error {
	if err := d.CheckCapacity(ctx, node); err != nil {
		return fmt.Errorf("refusing to drain %s: %w", node, err)
	}
	if err := d.Cordon(ctx, node); err != nil {
		return fmt.Errorf("cordon %s: %w", node, err)
	}
	pods, err := d.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return err
	}
	var onNode []corev1.Pod // the field selector is authoritative; re-check defensively
	for _, p := range pods.Items {
		if p.Spec.NodeName == node {
			onNode = append(onNode, p)
		}
	}
	toEvict, blockers := evictable(onNode)
	if len(blockers) > 0 {
		return fmt.Errorf("unmanaged pods block drain of %s: %s", node, strings.Join(blockers, ", "))
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pending := toEvict
	for len(pending) > 0 {
		var retry []corev1.Pod
		for _, p := range pending {
			err := d.Client.CoreV1().Pods(p.Namespace).EvictV1(ctx, &policyv1.Eviction{
				ObjectMeta:    metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
				DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: d.GracePeriodSeconds},
			})
			switch {
			case err == nil, apierrors.IsNotFound(err):
			case apierrors.IsTooManyRequests(err): // blocked by a PDB: try again later
				retry = append(retry, p)
			default:
				return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
		pending = retry
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain %s timed out with %d pod(s) blocked by PodDisruptionBudgets", node, len(pending))
		case <-time.After(d.poll()):
		}
	}
	return d.waitGone(ctx, toEvict)
}

func (d *Drainer) waitGone(ctx context.Context, pods []corev1.Pod) error {
	for _, p := range pods {
		for {
			cur, err := d.Client.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || (err == nil && cur.UID != p.UID) {
				break
			}
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting for %s/%s to terminate", p.Namespace, p.Name)
			case <-time.After(d.poll()):
			}
		}
	}
	return nil
}
