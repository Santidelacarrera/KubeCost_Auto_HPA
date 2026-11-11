// Package webhook implements the MutatingAdmissionWebhook for Pod resource
// right-sizing at admission time. This enables automated vertical optimization on
// Kubernetes clusters < 1.27 (where in-place pods/resize is unavailable) and
// eliminates cold-start over/under-provisioning when new Pods are scheduled.
package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execution"
)

// JSONPatchOperation represents a single RFC 6902 JSON Patch operation.
type JSONPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// ContainerRightSizeTarget specifies recommended admission-time resources for a container.
type ContainerRightSizeTarget struct {
	ContainerName        string
	RequestCPUMillicores int64
	LimitCPUMillicores   int64
	RequestMemoryMiB     int64
	LimitMemoryMiB       int64
}

// AdmissionMutationResult summarizes the webhook evaluation for a Pod creation event.
type AdmissionMutationResult struct {
	Allowed     bool
	Mutated     bool
	Patches     []JSONPatchOperation
	AuditReason string
}

// MutatePodOnAdmission evaluates a newly created Pod and generates RFC 6902 JSON patches
// when executionMode is ACTIVE or CANARY. In OBSERVE, SHADOW, DRY_RUN, or EMERGENCY_STOP
// modes, it records the audit recommendation without mutating the Pod.
func MutatePodOnAdmission(
	pod *corev1.Pod,
	target ContainerRightSizeTarget,
	mode execution.Mode,
) AdmissionMutationResult {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return AdmissionMutationResult{
			Allowed:     true,
			Mutated:     false,
			AuditReason: "pod has no containers; skipping admission mutation",
		}
	}

	normMode := execution.ParseMode(string(mode), false, false)
	if normMode != execution.ModeActive && normMode != execution.ModeCanary {
		return AdmissionMutationResult{
			Allowed: true,
			Mutated: false,
			AuditReason: fmt.Sprintf(
				"mode %s blocks admission mutation; would right-size container %q to %dm CPU / %dMi Memory",
				normMode, target.ContainerName, target.RequestCPUMillicores, target.RequestMemoryMiB,
			),
		}
	}

	containerIdx := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == target.ContainerName || (target.ContainerName == "" && i == 0) {
			containerIdx = i
			break
		}
	}
	if containerIdx < 0 {
		return AdmissionMutationResult{
			Allowed:     true,
			Mutated:     false,
			AuditReason: fmt.Sprintf("container %q not found in pod", target.ContainerName),
		}
	}

	c := &pod.Spec.Containers[containerIdx]
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}

	var patches []JSONPatchOperation
	if target.RequestCPUMillicores > 0 {
		q := *resource.NewMilliQuantity(target.RequestCPUMillicores, resource.DecimalSI)
		c.Resources.Requests[corev1.ResourceCPU] = q
		patches = append(patches, JSONPatchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("/spec/containers/%d/resources/requests/cpu", containerIdx),
			Value: q.String(),
		})
	}
	if target.LimitCPUMillicores > 0 {
		q := *resource.NewMilliQuantity(target.LimitCPUMillicores, resource.DecimalSI)
		c.Resources.Limits[corev1.ResourceCPU] = q
		patches = append(patches, JSONPatchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("/spec/containers/%d/resources/limits/cpu", containerIdx),
			Value: q.String(),
		})
	}
	if target.RequestMemoryMiB > 0 {
		q := *resource.NewQuantity(target.RequestMemoryMiB*1024*1024, resource.BinarySI)
		c.Resources.Requests[corev1.ResourceMemory] = q
		patches = append(patches, JSONPatchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("/spec/containers/%d/resources/requests/memory", containerIdx),
			Value: q.String(),
		})
	}
	if target.LimitMemoryMiB > 0 {
		q := *resource.NewQuantity(target.LimitMemoryMiB*1024*1024, resource.BinarySI)
		c.Resources.Limits[corev1.ResourceMemory] = q
		patches = append(patches, JSONPatchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("/spec/containers/%d/resources/limits/memory", containerIdx),
			Value: q.String(),
		})
	}

	return AdmissionMutationResult{
		Allowed: true,
		Mutated: len(patches) > 0,
		Patches: patches,
		AuditReason: fmt.Sprintf(
			"admission webhook right-sized container %q to %dm CPU / %dMi Memory",
			c.Name, target.RequestCPUMillicores, target.RequestMemoryMiB,
		),
	}
}

// PodMutator implements http.Handler for the /mutate-v1-pod AdmissionWebhook endpoint.
type PodMutator struct {
	DefaultExecutionMode execution.Mode
	DefaultTarget        ContainerRightSizeTarget
}

func (pm *PodMutator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var pod corev1.Pod
	if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res := MutatePodOnAdmission(&pod, pm.DefaultTarget, pm.DefaultExecutionMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
