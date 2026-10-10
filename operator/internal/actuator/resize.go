// Package actuator applies decisions to the cluster: in-place pod resizing
// and safe node cordon/drain.
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Pod condition types set by the kubelet during an in-place resize (Kubernetes >= 1.33).
const (
	podResizePending    corev1.PodConditionType = "PodResizePending"
	podResizeInProgress corev1.PodConditionType = "PodResizeInProgress"
)

var (
	// ErrWouldRestart is returned when the container's resizePolicy for CPU
	// requires a restart; the operator never restarts containers.
	ErrWouldRestart = errors.New("container resizePolicy for cpu requires restart")
	// ErrResizeInFlight is returned while a previous resize is still being applied.
	ErrResizeInFlight = errors.New("previous resize still in progress")
	// ErrContainerNotFound is returned when the pod lacks the target container.
	ErrContainerNotFound = errors.New("container not found in pod")
)

// Resizer patches pod CPU through the pods/resize subresource
// (InPlacePodVerticalScaling), so containers are not restarted.
type Resizer struct{ Client kubernetes.Interface }

// CurrentCPU returns the CPU request of the container, or zero.
func CurrentCPU(pod *corev1.Pod, container string) (resource.Quantity, bool) {
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			q, ok := c.Resources.Requests[corev1.ResourceCPU]
			return q, ok
		}
	}
	return resource.Quantity{}, false
}

// Target is the desired resource request of a container. Zero fields are left untouched.
type Target struct {
	CPUMillis   int64
	MemoryBytes int64
}

// ResizeCPU sets only the CPU request. See Resize.
func (r *Resizer) ResizeCPU(ctx context.Context, pod *corev1.Pod, container string, cpuMillis int64) (bool, error) {
	return r.Resize(ctx, pod, container, Target{CPUMillis: cpuMillis})
}

// Resize sets the requests of container through the pods/resize subresource.
// When a limit exists, the request/limit ratio is preserved. It returns
// changed=false when the pod already has the desired values.
func (r *Resizer) Resize(ctx context.Context, pod *corev1.Pod, container string, t Target) (changed bool, err error) {
	var c *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			c = &pod.Spec.Containers[i]
		}
	}
	if c == nil {
		return false, ErrContainerNotFound
	}
	reqs, lims := map[string]string{}, map[string]string{}
	add := func(name corev1.ResourceName, want int64, milli bool) error {
		if want <= 0 {
			return nil
		}
		cur, hasReq := c.Resources.Requests[name]
		curV := cur.Value()
		if milli {
			curV = cur.MilliValue()
		}
		if hasReq && curV == want {
			return nil
		}
		for _, p := range c.ResizePolicy {
			if p.ResourceName == name && p.RestartPolicy == corev1.RestartContainer {
				return fmt.Errorf("%w (%s)", ErrWouldRestart, name)
			}
		}
		fmtQ := func(v int64) string {
			if milli {
				return fmt.Sprintf("%dm", v)
			}
			return fmt.Sprintf("%d", v)
		}
		reqs[string(name)] = fmtQ(want)
		if lim, ok := c.Resources.Limits[name]; ok {
			limV := lim.Value()
			if milli {
				limV = lim.MilliValue()
			}
			newLim := limV
			if hasReq && curV > 0 {
				newLim = limV * want / curV
			}
			if newLim < want {
				newLim = want
			}
			lims[string(name)] = fmtQ(newLim)
		}
		return nil
	}
	if err := add(corev1.ResourceCPU, t.CPUMillis, true); err != nil {
		return false, err
	}
	if err := add(corev1.ResourceMemory, t.MemoryBytes, false); err != nil {
		return false, err
	}
	if len(reqs) == 0 {
		return false, nil
	}
	for _, cd := range pod.Status.Conditions {
		if (cd.Type == podResizePending || cd.Type == podResizeInProgress) && cd.Status == corev1.ConditionTrue {
			return false, ErrResizeInFlight
		}
	}
	res := map[string]any{"requests": reqs}
	if len(lims) > 0 {
		res["limits"] = lims
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": container, "resources": res},
	}}})
	if err != nil {
		return false, err
	}
	if _, err = r.Client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize"); err != nil {
		return false, fmt.Errorf("resize pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return true, nil
}

// SetReplicas scales a Deployment through its scale subresource.
func (r *Resizer) SetReplicas(ctx context.Context, namespace, name string, replicas int32) error {
	sc, err := r.Client.AppsV1().Deployments(namespace).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sc.Spec.Replicas == replicas {
		return nil
	}
	sc.Spec.Replicas = replicas
	_, err = r.Client.AppsV1().Deployments(namespace).UpdateScale(ctx, name, sc, metav1.UpdateOptions{})
	return err
}
