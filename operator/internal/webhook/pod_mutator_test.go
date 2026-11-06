package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execution"
)

func TestMutatePodOnAdmissionActiveVsShadow(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	target := ContainerRightSizeTarget{
		ContainerName:        "app",
		RequestCPUMillicores: 750,
		LimitCPUMillicores:   1500,
		RequestMemoryMiB:     512,
		LimitMemoryMiB:       1024,
	}

	// 1. SHADOW mode does not mutate Pod
	shadowRes := MutatePodOnAdmission(pod.DeepCopy(), target, execution.ModeShadow)
	if !shadowRes.Allowed || shadowRes.Mutated || len(shadowRes.Patches) != 0 {
		t.Fatalf("expected SHADOW mode to skip mutation, got %+v", shadowRes)
	}

	// 2. ACTIVE mode mutates Pod and emits RFC 6902 patches
	activePod := pod.DeepCopy()
	activeRes := MutatePodOnAdmission(activePod, target, execution.ModeActive)
	if !activeRes.Allowed || !activeRes.Mutated || len(activeRes.Patches) != 4 {
		t.Fatalf("expected ACTIVE mode to emit 4 patches, got %+v", activeRes)
	}
	gotCPU := activePod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue()
	if gotCPU != 750 {
		t.Fatalf("expected mutated CPU request 750m, got %dm", gotCPU)
	}
}
