package controller

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
)

// A KubeCostAutoscaler accepts predictive, oomProtection and nodeBinPacking in its schema and implements none of them. The
// status must say so, once, instead of letting the user believe they run.
func TestUnsupportedFieldsAreReportedNotSilentlyIgnored(t *testing.T) {
	a := baseAutoscaler()
	a.Spec.Predictive = &v1alpha1.PredictivePolicy{Enabled: true}
	a.Spec.NodeBinPacking = &v1alpha1.NodeBinPackingPolicy{Enabled: true}
	a.Spec.OOMProtection = &v1alpha1.OOMProtectionPolicy{Enabled: false} // present but off: not reported
	e := newKCAEnv(t, a, 6)
	e.res.util = 0.7
	e.mustReconcile()

	e.requireCond(v1alpha1.ConditionUnsupportedFields, metav1.ConditionTrue, "Ignored")
	c := meta.FindStatusCondition(e.get().Status.Conditions, v1alpha1.ConditionUnsupportedFields)
	if !strings.Contains(c.Message, "spec.predictive") || !strings.Contains(c.Message, "spec.nodeBinPacking") || strings.Contains(c.Message, "spec.oomProtection") {
		t.Fatalf("message must name exactly the enabled fields: %s", c.Message)
	}
	warned := false
	for _, ev := range e.events() {
		if strings.Contains(ev, "Warning") && strings.Contains(ev, "UnsupportedFields=True") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("a Warning event must be emitted the first time")
	}
	// the rest of the controller is unaffected, and the warning is not repeated
	e.requireCond("Ready", metav1.ConditionTrue, "Reconciled")
	e.requireCond("ScalingActive", metav1.ConditionTrue, "Owner")
	for i := 0; i < 5; i++ {
		e.advance(30 * time.Second)
		e.mustReconcile()
	}
	if evs := e.events(); len(evs) != 0 {
		t.Fatalf("repeated events: %v", evs)
	}
}

func TestNoUnsupportedFieldsConditionOnAPlainSpec(t *testing.T) {
	e := newKCAEnv(t, baseAutoscaler(), 6)
	e.res.util = 0.7
	e.mustReconcile()
	if c := meta.FindStatusCondition(e.get().Status.Conditions, v1alpha1.ConditionUnsupportedFields); c != nil {
		t.Fatalf("a spec without those fields must not grow a condition: %+v", c)
	}
}
