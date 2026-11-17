package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func wrecWith(name, phase string, rec recommend.Recommendation) *v1alpha1.WorkloadRecommendation {
	b, _ := json.Marshal(rec)
	return &v1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec:       v1alpha1.WorkloadRecommendationSpec{RecommendationID: name, Record: string(b)},
		Status:     v1alpha1.WorkloadRecommendationStatus{Phase: phase},
	}
}

func dec(cpuFrom, cpuTo, memFrom, memTo, usd float64) recommend.Recommendation {
	return recommend.Recommendation{
		CPU:    recommend.ResourceResult{Action: recommend.Decrease, Current: cpuFrom, Recommended: cpuTo, PotentialMonthlyUSD: usd},
		Memory: recommend.ResourceResult{Action: recommend.Decrease, Current: memFrom, Recommended: memTo},
	}
}

// The outcome gauges come from the objects themselves: only Verified decreases count as kept, rolled-back and denied ones do not,
// ratios follow the documented definitions, and a list failure is visible instead of reporting zeros.
func TestOutcomeCollectorReportsWhatTheObjectsSay(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	objs := []runtimeObj{
		wrecWith("a", v1alpha1.RecPhaseVerified, dec(0.5, 0.25, 256<<20, 128<<20, 10)),
		wrecWith("b", v1alpha1.RecPhaseVerified, dec(1, 0.5, 0, 0, 5)),
		wrecWith("c", v1alpha1.RecPhaseRolledBack, dec(2, 1, 0, 0, 99)),
		wrecWith("d", v1alpha1.RecPhaseDenied, dec(2, 1, 0, 0, 99)),
		wrecWith("e", "", dec(2, 1, 0, 0, 99)),
		wrecWith("f", v1alpha1.RecPhaseDryRun, dec(2, 1, 0, 0, 99)),
	}
	b := crfake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	c := NewOutcomeCollector(b.Build())
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	want := `
# HELP kubecost_recommendation_acceptance_ratio Applied / (applied + denied) over current WorkloadRecommendation objects; absent until one was decided.
# TYPE kubecost_recommendation_acceptance_ratio gauge
kubecost_recommendation_acceptance_ratio 0.75
# HELP kubecost_recommendation_rollback_ratio RolledBack / (Verified + RolledBack) over current WorkloadRecommendation objects; absent until one finished.
# TYPE kubecost_recommendation_rollback_ratio gauge
kubecost_recommendation_rollback_ratio 0.3333333333333333
# HELP kubecost_recommendation_kept_estimated_monthly_usd ESTIMATED monthly saving of verified decreases (requests delta x supplied unit prices). Not a bill and not an observed saving.
# TYPE kubecost_recommendation_kept_estimated_monthly_usd gauge
kubecost_recommendation_kept_estimated_monthly_usd 15
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kubecost_recommendation_acceptance_ratio",
		"kubecost_recommendation_rollback_ratio", "kubecost_recommendation_kept_estimated_monthly_usd"); err != nil {
		t.Fatal(err)
	}
	tot := Totals(listOf(objs))
	if tot.CPUCores != 0.75 || tot.MemBytes != 128<<20 || tot.ByPhase[v1alpha1.RecPhasePending] != 1 || tot.ByPhase[v1alpha1.RecPhaseFailed] != 0 {
		t.Fatalf("%+v", tot)
	}
	if n := testutil.CollectAndCount(c, "kubecost_recommendations"); n != 7 {
		t.Fatalf("every phase is exported, zeros included: %d", n)
	}
	if v := testutil.ToFloat64(prometheus.CollectorFunc(func(ch chan<- prometheus.Metric) {
		NewOutcomeCollector(crfake.NewClientBuilder().Build()).Collect(ch) // no scheme for the type: List fails
	})); v != 0 {
		t.Fatalf("a failed list must report success 0, got %v", v)
	}
}

func TestOutcomeTotalsWithoutDecisionsHasNoRatios(t *testing.T) {
	tot := Totals(listOf([]runtimeObj{wrecWith("p", v1alpha1.RecPhasePending, dec(1, 0.5, 0, 0, 1))}))
	if tot.HasAcceptance || tot.HasRollback || tot.CPUCores != 0 {
		t.Fatalf("%+v", tot)
	}
	broken := wrecWith("x", v1alpha1.RecPhaseVerified, recommend.Recommendation{})
	broken.Spec.Record = "{not json"
	if Totals(listOf([]runtimeObj{broken})).UnreadableVerifiedRecords != 1 {
		t.Fatal("an unreadable record must be counted, not ignored")
	}
}

type runtimeObj = *v1alpha1.WorkloadRecommendation

func listOf(o []runtimeObj) []v1alpha1.WorkloadRecommendation {
	out := make([]v1alpha1.WorkloadRecommendation, len(o))
	for i := range o {
		out[i] = *o[i]
	}
	return out
}
