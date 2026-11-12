package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend/promsrc"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/safety"
)

var errTerminal = errors.New("the recommendation already reached a final phase")

// PolicyKey is the ConfigMap key holding the safety.Policy; "slo.<name>" keys hold the named SLO breach queries.
// A key "policy.<namespace>.yaml" replaces PolicyKey for that namespace (per-team limits); it must itself allow the namespace.
const PolicyKey = "policy.yaml"

// RecommendationApplier applies approved WorkloadRecommendations. It is OPT-IN (--enable-recommendation-applier), and every gate of
// the CLI path applies to it unchanged, because it runs the same execute.Executor and safety.Evaluate:
//
//   - a policy ConfigMap that names the allowed namespaces (no ConfigMap, no change; read again on every reconcile, so the kill
//     switch takes effect immediately);
//   - a human approval written to status.approval through the status subresource (its own RBAC permission), for this exact ID;
//   - the workload must be in the SAME namespace as the recommendation object;
//   - observe-only mode (the chart default) turns every decision into a report;
//   - the usual refusals (GitOps-managed, HPA on CPU, VPA, stale, limits, rate limit, windows) and automatic rollback.
type RecommendationApplier struct {
	client.Client
	KC       kubernetes.Interface
	Dyn      dynamic.Interface
	Recorder record.EventRecorder
	// APIReader reads straight from the API server, never from the informer cache. A reconcile that was queued while the previous one was
	// still running can see a cached object that is one status update behind; deciding on it would re-evaluate a change that already ended
	// (found on a real cluster: a finished RolledBack was overwritten by Denied). If nil, the cached client is used (unit tests).
	APIReader client.Reader

	PolicyNamespace, PolicyName string
	ObserveOnly                 bool
	// Prom, if set, evaluates the named SLO queries during observation.
	Prom *promsrc.Client
	// MaxObserve and MaxRollout cap what a recommendation may ask for.
	MaxObserve, MaxRollout time.Duration
	Now                    func() time.Time
	Sleep                  func(time.Duration)
}

func (r *RecommendationApplier) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RBAC for this controller is deliberately NOT generated from markers into the default manager role: it is opt-in and needs
// "update" on workloads, so it lives in config/rbac/applier_role.yaml and in the Helm chart under operator.recommendationApplier.

// Reconcile advances one recommendation.
func (r *RecommendationApplier) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	var cr v1alpha1.WorkloadRecommendation
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.APIReader != nil { // decide on the live object, not on a possibly stale cached copy
		if err := r.APIReader.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	switch cr.Status.Phase {
	case v1alpha1.RecPhaseVerified, v1alpha1.RecPhaseRolledBack, v1alpha1.RecPhaseFailed:
		return ctrl.Result{}, nil // terminal: a new decision needs a new recommendation
	}

	rec, err := r.parse(&cr)
	if err != nil {
		return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, err.Error(), []string{err.Error()}, 0)
	}
	ap := cr.Status.Approval
	if ap == nil || ap.RecommendationID != cr.Spec.RecommendationID || ap.By == "" {
		return r.set(ctx, &cr, v1alpha1.RecPhasePending, "waiting for an approval of this exact recommendation (rightsize approve --cr)", nil, false)
	}

	pol, slo, err := r.policy(ctx, cr.Namespace)
	if err != nil {
		lg.Info("no usable policy: nothing will be applied", "error", err.Error())
		return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, "no usable policy: "+err.Error(), []string{err.Error()}, time.Minute)
	}
	ex := &execute.Executor{KC: r.KC, Dyn: r.Dyn, Audit: &crSink{r: r.Recorder, obj: &cr}, Actor: "operator/recommendation-applier", Now: r.Now, Sleep: r.Sleep,
		ApprovalOverride: func(_, _, _ string) (string, string, bool) { return ap.RecommendationID, ap.By, true }}

	th := execute.Thresholds{ObserveFor: capDur(5*time.Minute, r.MaxObserve), RolloutTimeout: capDur(5*time.Minute, r.MaxRollout)}
	if cr.Spec.SLOQueryName != "" {
		q, ok := slo[cr.Spec.SLOQueryName]
		switch {
		case !ok:
			return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, fmt.Sprintf("the SLO query %q is not defined in the policy ConfigMap", cr.Spec.SLOQueryName), nil, time.Minute)
		case r.Prom == nil:
			return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, "an SLO query was requested but the operator has no Prometheus configured", nil, 0)
		default:
			th.SLO = func(ctx context.Context) (bool, bool, error) { return r.Prom.Breached(ctx, q, r.now()) }
		}
	}

	// An interrupted run: the change is already on the workload. Resume watching; never apply twice.
	if cr.Status.Phase == v1alpha1.RecPhaseApplying {
		if facts, _, ierr := ex.Inspect(ctx, rec.Kind, rec.Namespace, rec.Workload); ierr == nil && facts.AlreadyApplied == rec.ID {
			lg.Info("resuming the observation of an applied change", "id", rec.ID)
			return r.afterWatch(ctx, &cr, rec, ex, th)
		}
	}

	dry, err := ex.Apply(ctx, rec, pol, false)
	if err != nil {
		return r.finish(ctx, &cr, v1alpha1.RecPhaseFailed, "evaluation failed: "+err.Error(), nil, 0)
	}
	if !dry.Decision.Allowed {
		// a maintenance window or rate limit may open later: look again
		return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, "the policy refused it", dry.Decision.Reasons, 5*time.Minute)
	}
	if r.ObserveOnly {
		return r.finish(ctx, &cr, v1alpha1.RecPhaseDryRun, "observe-only mode: approved and allowed, but nothing was written", nil, 0)
	}

	if _, err := r.set(ctx, &cr, v1alpha1.RecPhaseApplying, "applying and observing", nil, false); err != nil {
		return ctrl.Result{}, err
	}
	res, err := ex.Apply(ctx, rec, pol, true)
	if err != nil {
		return r.finish(ctx, &cr, v1alpha1.RecPhaseFailed, "apply failed (nothing was changed or the state is recorded on the workload): "+err.Error(), nil, 0)
	}
	if !res.Applied {
		return r.finish(ctx, &cr, v1alpha1.RecPhaseDenied, "the policy refused it at write time", res.Decision.Reasons, 5*time.Minute)
	}
	t := metav1.NewTime(r.now())
	cr.Status.AppliedAt = &t
	return r.afterWatch(ctx, &cr, rec, ex, th)
}

func (r *RecommendationApplier) afterWatch(ctx context.Context, cr *v1alpha1.WorkloadRecommendation, rec recommend.Recommendation, ex *execute.Executor, th execute.Thresholds) (ctrl.Result, error) {
	started := r.now()
	if cr.Status.AppliedAt != nil {
		started = cr.Status.AppliedAt.Time
	}
	v, err := ex.WatchAndRollback(ctx, rec, th)
	saving := rec.CPU.PotentialMonthlyUSD + rec.Memory.PotentialMonthlyUSD
	if ctx.Err() == nil {
		mRecApplySeconds.Observe(r.now().Sub(started).Seconds())
		switch {
		case err == nil && v.Healthy && saving > 0:
			mRecSavings.WithLabelValues("kept").Add(saving)
		case err == nil && !v.Healthy && saving > 0:
			mRecSavings.WithLabelValues("rolled_back").Add(saving)
		}
	}
	switch {
	case ctx.Err() != nil:
		// shutting down: leave the phase as Applying so the next process resumes
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	case err != nil:
		return r.finish(ctx, cr, v1alpha1.RecPhaseFailed, "a human must look: "+err.Error(), v.Reasons, 0)
	case v.Healthy:
		return r.finish(ctx, cr, v1alpha1.RecPhaseVerified, "applied and healthy for the whole observation window", nil, 0)
	default:
		return r.finish(ctx, cr, v1alpha1.RecPhaseRolledBack, "degraded after the change and restored", v.Reasons, 0)
	}
}

func capDur(def, max time.Duration) time.Duration {
	if max > 0 && def > max {
		return max
	}
	return def
}

// parse checks that the stored record is the recommendation the object says it is, and that it targets this namespace only.
func (r *RecommendationApplier) parse(cr *v1alpha1.WorkloadRecommendation) (recommend.Recommendation, error) {
	var rec recommend.Recommendation
	if err := json.Unmarshal([]byte(cr.Spec.Record), &rec); err != nil {
		return rec, fmt.Errorf("spec.record is not a recommendation: %v", err)
	}
	t := cr.Spec.Target
	switch {
	case rec.ID != cr.Spec.RecommendationID:
		return rec, fmt.Errorf("spec.record has ID %q but spec.recommendationID is %q", rec.ID, cr.Spec.RecommendationID)
	case rec.Namespace != cr.Namespace:
		return rec, fmt.Errorf("the record targets namespace %q but this object lives in %q: a recommendation can only act in its own namespace", rec.Namespace, cr.Namespace)
	case rec.Kind != t.Kind || rec.Workload != t.Name || rec.Container != t.Container:
		return rec, fmt.Errorf("spec.target (%s %s/%s) does not match the record (%s %s/%s)", t.Kind, t.Name, t.Container, rec.Kind, rec.Workload, rec.Container)
	}
	return rec, nil
}

// policy reads the ConfigMap fresh each time (the kill switch must not need a restart).
func (r *RecommendationApplier) policy(ctx context.Context, namespace string) (safety.Policy, map[string]string, error) {
	var pol safety.Policy
	if r.PolicyName == "" {
		return pol, nil, errors.New("no policy ConfigMap is configured (--applier-policy-configmap)")
	}
	// A direct GET by name, not the manager's cached client: the cache would LIST and WATCH every ConfigMap, which the Role
	// (get on this one name) rightly does not allow. Found on a real cluster; fake clients cannot show it.
	cm, err := r.KC.CoreV1().ConfigMaps(r.PolicyNamespace).Get(ctx, r.PolicyName, metav1.GetOptions{})
	if err != nil {
		return pol, nil, fmt.Errorf("policy ConfigMap %s/%s: %v", r.PolicyNamespace, r.PolicyName, err)
	}
	key := PolicyKey
	if _, ok := cm.Data["policy."+namespace+".yaml"]; ok {
		key = "policy." + namespace + ".yaml" // this namespace's own policy replaces the global one entirely
	}
	raw, ok := cm.Data[key]
	if !ok {
		return pol, nil, fmt.Errorf("policy ConfigMap %s/%s has no %q key", r.PolicyNamespace, r.PolicyName, key)
	}
	if err := yaml.UnmarshalStrict([]byte(raw), &pol); err != nil {
		return pol, nil, fmt.Errorf("policy is not valid: %v", err)
	}
	if err := pol.Validate(); err != nil {
		return pol, nil, err
	}
	slo := map[string]string{}
	for k, v := range cm.Data {
		if len(k) > 4 && k[:4] == "slo." {
			slo[k[4:]] = v
		}
	}
	return pol, slo, nil
}

// set writes the phase (and optionally finishes).
func (r *RecommendationApplier) set(ctx context.Context, cr *v1alpha1.WorkloadRecommendation, phase, msg string, reasons []string, finished bool) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(cr)
	appliedAt := cr.Status.AppliedAt
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur v1alpha1.WorkloadRecommendation
		reader := client.Reader(r.Client)
		if r.APIReader != nil {
			reader = r.APIReader
		}
		if err := reader.Get(ctx, key, &cur); err != nil {
			return err
		}
		switch cur.Status.Phase { // a finished outcome is never overwritten, whatever raced with it
		case v1alpha1.RecPhaseVerified, v1alpha1.RecPhaseRolledBack, v1alpha1.RecPhaseFailed:
			if cur.Status.Phase != phase {
				return errTerminal
			}
		}
		cur.Status.ObservedGeneration = cur.Generation
		cur.Status.Phase, cur.Status.Message, cur.Status.Reasons = phase, msg, reasons
		if appliedAt != nil {
			cur.Status.AppliedAt = appliedAt
		}
		if finished {
			t := metav1.NewTime(r.now())
			cur.Status.FinishedAt = &t
		}
		cond := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: phase, Message: msg, ObservedGeneration: cur.Generation}
		if phase == v1alpha1.RecPhaseVerified {
			cond.Status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&cur.Status.Conditions, cond)
		return r.Status().Update(ctx, &cur)
	})
	if err != nil {
		if apierrors.IsNotFound(err) || errors.Is(err, errTerminal) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	mRecPhase.WithLabelValues(phase).Inc()
	if r.Recorder != nil {
		et := corev1.EventTypeNormal
		if phase == v1alpha1.RecPhaseFailed || phase == v1alpha1.RecPhaseRolledBack {
			et = corev1.EventTypeWarning
		}
		r.Recorder.Event(cr, et, phase, truncate(msg, 300))
	}
	return ctrl.Result{}, nil
}

func (r *RecommendationApplier) finish(ctx context.Context, cr *v1alpha1.WorkloadRecommendation, phase, msg string, reasons []string, requeue time.Duration) (ctrl.Result, error) {
	terminal := phase == v1alpha1.RecPhaseVerified || phase == v1alpha1.RecPhaseRolledBack || phase == v1alpha1.RecPhaseFailed
	// do not rewrite an identical Denied/Pending status on every look: it would loop through the watch
	if cr.Status.Phase == phase && cr.Status.Message == msg && !terminal {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if _, err := r.set(ctx, cr, phase, msg, reasons, terminal); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// SetupWithManager registers the controller. Two workers: an observation can last minutes.
func (r *RecommendationApplier) SetupWithManager(mgr ctrl.Manager) error {
	if err := RegisterOutcomeCollector(mgr.GetClient()); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkloadRecommendation{}).
		Named("recommendation-applier").
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}

// crSink turns the executor's audit events into Kubernetes Events on the recommendation object.
type crSink struct {
	r   record.EventRecorder
	obj runtime.Object
}

func (s *crSink) Record(e execute.Event) {
	if s.r == nil {
		return
	}
	et := corev1.EventTypeNormal
	switch e.Type {
	case "degraded", "rolled_back", "apply_failed", "rollback_failed", "inspect_failed":
		et = corev1.EventTypeWarning
	}
	if e.Type == "decision" {
		return // the phase and reasons already carry it
	}
	s.r.Event(s.obj, et, "Audit_"+e.Type, truncate(fmt.Sprintf("%v", e.Details), 300))
}
