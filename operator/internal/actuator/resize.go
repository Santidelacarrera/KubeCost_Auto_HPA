// Package actuator applies decisions to the cluster: in-place pod resizing
// and safe node cordon/drain.
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

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

// ResizeState classifies the real outcome of an in-place pod resize operation.
type ResizeState string

const (
	ResizeStatusAccepted   ResizeState = "Accepted"
	ResizeStatusPending    ResizeState = "Pending"
	ResizeStatusDeferred   ResizeState = "Deferred"
	ResizeStatusInfeasible ResizeState = "Infeasible"
	ResizeStatusInProgress ResizeState = "InProgress"
	ResizeStatusCompleted  ResizeState = "Completed"
	ResizeStatusRejected   ResizeState = "Rejected"
)

var (
	// ErrWouldRestart is returned when the container's resizePolicy requires a restart.
	ErrWouldRestart = errors.New("container resizePolicy for cpu requires restart")
	// ErrResizeInFlight is returned while a previous resize is still being applied.
	ErrResizeInFlight = errors.New("previous resize still in progress")
	// ErrResizeDeferred wraps ErrResizeInFlight when the kubelet temporarily defers the resize (e.g., node pressure).
	ErrResizeDeferred = fmt.Errorf("%w: resize deferred by kubelet", ErrResizeInFlight)
	// ErrResizeInfeasible is returned when the kubelet declares the requested resize impossible on the current node.
	ErrResizeInfeasible = errors.New("pod resize is infeasible on current node")
	// ErrContainerNotFound is returned when the pod lacks the target container.
	ErrContainerNotFound = errors.New("container not found in pod")
)

// K8sResizeCapability describes the cluster's support for pods/resize.
type K8sResizeCapability struct {
	Major                int
	Minor                int
	SubresourceAvailable bool
	Stage                string // "Unsupported", "Alpha", "Beta", "GA"
	FeatureGateRequired  bool
	SupportsInPlace      bool
	Explanation          string
}

// DetectResizeCapability evaluates Kubernetes API version and subresource presence.
// In-Place Pod Vertical Scaling is Beta from Kubernetes 1.33 and GA/Stable from 1.35.
func DetectResizeCapability(majorStr, minorStr string, subresourceAvailable bool) K8sResizeCapability {
	cleanMinor := strings.TrimRight(minorStr, "+")
	maj, _ := strconv.Atoi(majorStr)
	min, _ := strconv.Atoi(cleanMinor)

	cap := K8sResizeCapability{
		Major:                maj,
		Minor:                min,
		SubresourceAvailable: subresourceAvailable,
	}
	if maj < 1 || (maj == 1 && min < 27) {
		cap.Stage = "Unsupported"
		cap.SupportsInPlace = false
		cap.Explanation = fmt.Sprintf("Kubernetes %d.%d does not support in-place pod vertical scaling", maj, min)
		return cap
	}
	if maj == 1 && min < 33 {
		cap.Stage = "Alpha"
		cap.FeatureGateRequired = true
		cap.SupportsInPlace = subresourceAvailable
		cap.Explanation = fmt.Sprintf("Kubernetes %d.%d has Alpha InPlacePodVerticalScaling (requires explicit feature gate)", maj, min)
		return cap
	}
	if maj == 1 && min < 35 {
		cap.Stage = "Beta"
		cap.FeatureGateRequired = false
		cap.SupportsInPlace = subresourceAvailable
		cap.Explanation = fmt.Sprintf("Kubernetes %d.%d supports Beta InPlacePodVerticalScaling via pods/resize", maj, min)
		return cap
	}
	cap.Stage = "GA"
	cap.FeatureGateRequired = false
	cap.SupportsInPlace = subresourceAvailable
	cap.Explanation = fmt.Sprintf("Kubernetes %d.%d has Stable/GA InPlacePodVerticalScaling support", maj, min)
	return cap
}

// InspectResizeStatus inspects the live Pod status conditions and container status resources
// to verify whether a requested resize was Completed, Deferred, Infeasible, InProgress, or Accepted.
func InspectResizeStatus(pod *corev1.Pod, container string, desired Target) (ResizeState, string) {
	for _, cd := range pod.Status.Conditions {
		if cd.Type == podResizePending && cd.Status == corev1.ConditionTrue {
			switch strings.ToLower(cd.Reason) {
			case "infeasible":
				return ResizeStatusInfeasible, fmt.Sprintf("kubelet rejected resize as Infeasible: %s", cd.Message)
			case "deferred":
				return ResizeStatusDeferred, fmt.Sprintf("kubelet deferred resize (temporary capacity constraint): %s", cd.Message)
			default:
				return ResizeStatusPending, fmt.Sprintf("resize pending (%s): %s", cd.Reason, cd.Message)
			}
		}
		if cd.Type == podResizeInProgress && cd.Status == corev1.ConditionTrue {
			if strings.HasPrefix(strings.ToLower(cd.Reason), "error") {
				return ResizeStatusRejected, fmt.Sprintf("kubelet resize error (%s): %s", cd.Reason, cd.Message)
			}
			return ResizeStatusInProgress, fmt.Sprintf("kubelet applying cgroup changes: %s", cd.Message)
		}
	}

	// Check containerStatuses to verify if the kubelet has actually applied the desired resources
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != container {
			continue
		}
		if cs.Resources == nil {
			break
		}
		cpuOK := true
		if desired.CPUMillis > 0 {
			appliedCPU, ok := cs.Resources.Requests[corev1.ResourceCPU]
			cpuOK = ok && appliedCPU.MilliValue() == desired.CPUMillis
		}
		memOK := true
		if desired.MemoryBytes > 0 {
			appliedMem, ok := cs.Resources.Requests[corev1.ResourceMemory]
			memOK = ok && appliedMem.Value() == desired.MemoryBytes
		}
		if cpuOK && memOK {
			return ResizeStatusCompleted, "kubelet verified containerStatuses.resources match desired target"
		}
		return ResizeStatusAccepted, "API Server accepted spec update; awaiting kubelet containerStatus actuation"
	}

	return ResizeStatusAccepted, "API Server accepted spec update; containerStatuses.resources not yet reported"
}

// Resizer patches pod CPU/Memory through the pods/resize subresource
// (InPlacePodVerticalScaling), so containers are not restarted when resizePolicy permits.
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
		if cd.Type == podResizePending && cd.Status == corev1.ConditionTrue {
			if strings.EqualFold(cd.Reason, "Infeasible") {
				return false, fmt.Errorf("%w: %s", ErrResizeInfeasible, cd.Message)
			}
			if strings.EqualFold(cd.Reason, "Deferred") {
				return false, fmt.Errorf("%w: %s", ErrResizeDeferred, cd.Message)
			}
			return false, ErrResizeInFlight
		}
		if cd.Type == podResizeInProgress && cd.Status == corev1.ConditionTrue {
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
