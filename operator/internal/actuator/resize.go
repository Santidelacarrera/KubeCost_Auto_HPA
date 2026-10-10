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

// ResizeCPU sets the CPU request of container to cpuMillis. If the container
// has a CPU limit, the request/limit ratio is preserved. Memory is untouched
// (shrinking memory in place is unsafe). It returns changed=false when the pod
// already has the desired value.
func (r *Resizer) ResizeCPU(ctx context.Context, pod *corev1.Pod, container string, cpuMillis int64) (changed bool, err error) {
	var target *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			target = &pod.Spec.Containers[i]
		}
	}
	if target == nil {
		return false, ErrContainerNotFound
	}
	if cur, ok := target.Resources.Requests[corev1.ResourceCPU]; ok && cur.MilliValue() == cpuMillis {
		return false, nil
	}
	for _, p := range target.ResizePolicy {
		if p.ResourceName == corev1.ResourceCPU && p.RestartPolicy == corev1.RestartContainer {
			return false, ErrWouldRestart
		}
	}
	for _, c := range pod.Status.Conditions {
		if (c.Type == podResizePending || c.Type == podResizeInProgress) && c.Status == corev1.ConditionTrue {
			return false, ErrResizeInFlight
		}
	}

	res := map[string]any{"requests": map[string]string{"cpu": fmt.Sprintf("%dm", cpuMillis)}}
	if lim, ok := target.Resources.Limits[corev1.ResourceCPU]; ok {
		newLimit := lim.MilliValue()
		if req, ok := target.Resources.Requests[corev1.ResourceCPU]; ok && req.MilliValue() > 0 {
			newLimit = lim.MilliValue() * cpuMillis / req.MilliValue()
		}
		if newLimit < cpuMillis {
			newLimit = cpuMillis
		}
		res["limits"] = map[string]string{"cpu": fmt.Sprintf("%dm", newLimit)}
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": container, "resources": res},
	}}})
	if err != nil {
		return false, err
	}
	_, err = r.Client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")
	if err != nil {
		return false, fmt.Errorf("resize pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return true, nil
}
