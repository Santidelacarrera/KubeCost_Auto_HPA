package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/kubecost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/ownership"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/resmetrics"
)

// targetIndex indexes autoscalers by the Deployment they target.
const targetIndex = "spec.targetRef.name"

const (
	maxBackoff           = 5 * time.Minute
	costCacheTTL         = 20 * time.Second
	maxRecommendationLen = 512
)

// CostSource is the KubeCost client abstraction.
type CostSource interface {
	Allocation(ctx context.Context, q kubecost.Query) (kubecost.Sample, error)
}

// KubeCostAutoscalerReconciler is the SOLE writer of the target Deployment's
// replicas for the resources it manages (see ownership for conflict handling).
//
// Responsibilities are split: kubecost (I/O), resmetrics (I/O), infra
// (classification), policy (pure decisions), actuator (writes) and this
// reconciler (orchestration, status, failure policy).
type KubeCostAutoscalerReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Cost      CostSource
	Resources resmetrics.Reader
	Scaler    actuator.ReplicaScaler
	Fallback  *ownership.Fallback
	Recorder  record.EventRecorder
	Now       func() time.Time
	Jitter    func() float64

	mu        sync.Mutex
	costCache map[types.NamespacedName]cachedCost
}

type cachedCost struct {
	sample kubecost.Sample
	err    error
	at     time.Time
}

func (r *KubeCostAutoscalerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecostautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecostautoscalers/status,verbs=get;update
// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecosthpas,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update
// +kubebuilder:rbac:groups="",resources=pods;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=list
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile runs one evaluation of one autoscaler.
func (r *KubeCostAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	start := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		mReconcileSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
	}()
	return r.reconcile(ctx, req)
}

func (r *KubeCostAutoscalerReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	var a v1alpha1.KubeCostAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		if apierrors.IsNotFound(err) {
			forgetSeries(req.Namespace, req.Name)
			r.forgetCost(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !a.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	now := r.now()
	st := a.Status.DeepCopy()
	st.ObservedGeneration = a.Generation
	run := &runState{r: r, a: &a, st: st, now: now}

	s, err := Resolve(&a.Spec)
	if err != nil {
		run.cond(v1alpha1.ConditionReady, false, "InvalidSpec", err.Error())
		run.cond(v1alpha1.ConditionScalingActive, false, "InvalidSpec", "the spec is invalid; no scaling is performed")
		st.ScalingOwner = v1alpha1.OwnerNone
		return ctrl.Result{}, r.persist(ctx, &a, st)
	}
	if a.Annotations[v1alpha1.AnnotationPause] == "true" {
		s.DryRun, s.Paused = true, true
	}
	if prev := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionScalingActive); s.Paused && (prev == nil || prev.Reason != "Paused") {
		mPauses.WithLabelValues(a.Namespace, a.Name).Inc()
	}
	paused := 0.0
	if s.Paused {
		paused = 1
	}
	mPaused.WithLabelValues(a.Namespace, a.Name).Set(paused)
	run.s = s

	// --- target ---
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.TargetRef.Name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			// Nothing to scale; drop the fallback HPA so it does not dangle.
			if st.Fallback.Active {
				_, _ = r.Fallback.Remove(ctx, &a)
				st.Fallback = v1alpha1.FallbackStatus{}
			}
			run.cond(v1alpha1.ConditionReady, false, "TargetNotFound", fmt.Sprintf("Deployment %q not found", a.Spec.TargetRef.Name))
			run.cond(v1alpha1.ConditionScalingActive, false, "TargetNotFound", "no target to scale")
			st.ScalingOwner = v1alpha1.OwnerNone
			if perr := r.persist(ctx, &a, st); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: s.Interval}, nil
		}
		run.cond(v1alpha1.ConditionReady, false, "TargetUnreadable", "the Kubernetes API is unavailable")
		_ = r.persist(ctx, &a, st)
		return ctrl.Result{}, fmt.Errorf("get deployment: %w", err)
	}
	current := int32(1)
	if dep.Spec.Replicas != nil {
		current = *dep.Spec.Replicas
	}
	// st.CurrentReplicas holds the last observation, or the count this controller wrote, so a
	// difference here is a change made by someone else.
	if prev := st.CurrentReplicas; prev != 0 && prev != current {
		t := metav1.NewTime(now)
		st.LastReplicaChangeTime = &t
	}
	st.CurrentReplicas, st.ReadyReplicas = current, dep.Status.ReadyReplicas
	setReplicaMetrics(&a, current)

	// --- single-writer guarantee ---
	conflicts, err := ownership.ForAutoscaler(ctx, r.Client, &a)
	if err != nil {
		run.cond(v1alpha1.ConditionReady, false, "OwnershipCheckFailed", "could not verify that no other controller scales the target")
		_ = r.persist(ctx, &a, st)
		return ctrl.Result{}, err
	}
	if len(conflicts) > 0 {
		msg := conflicts[0].String()
		run.cond(v1alpha1.ConditionReady, false, "Conflict", msg)
		run.cond(v1alpha1.ConditionScalingActive, false, "Conflict", "another controller owns the replicas: "+msg)
		st.ScalingOwner = v1alpha1.OwnerNone
		if perr := r.persist(ctx, &a, st); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: s.Interval}, nil
	}
	// A fallback HPA that we own must always be reflected in status, even if the
	// status was lost: otherwise the controller would write replicas while the HPA does too.
	if !st.Fallback.Active {
		if h, found, gerr := r.Fallback.Get(ctx, &a); gerr != nil {
			return ctrl.Result{}, gerr
		} else if found {
			since := h.CreationTimestamp
			st.Fallback = v1alpha1.FallbackStatus{Active: true, Since: &since, HPAName: h.Name}
		}
	}
	if current == 0 {
		// Same convention as the native HPA: zero replicas means "autoscaling disabled".
		run.cond(v1alpha1.ConditionReady, true, "ScaledToZero", "the Deployment is scaled to zero; autoscaling is paused")
		run.cond(v1alpha1.ConditionScalingActive, false, "ScaledToZero", "autoscaling is paused while replicas is 0")
		st.ScalingOwner = v1alpha1.OwnerNone
		return ctrl.Result{RequeueAfter: s.Interval}, r.persist(ctx, &a, st)
	}

	// --- observe ---
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		run.cond(v1alpha1.ConditionReady, false, "InvalidSelector", "the target has an invalid selector")
		return ctrl.Result{}, r.persist(ctx, &a, st)
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(a.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		run.cond(v1alpha1.ConditionReady, false, "PodsUnreadable", "the Kubernetes API is unavailable")
		_ = r.persist(ctx, &a, st)
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}
	summary := s.Classifier.Summarize(podList.Items, r.nodeLookup(ctx))
	st.Infrastructure = v1alpha1.InfrastructureStatus{Spot: summary.Spot, OnDemand: summary.OnDemand, Unknown: summary.Unknown, Pending: summary.Pending, Unschedulable: summary.Unschedulable}
	setNodeMetrics(&a, summary)

	demand, derr := r.demand(ctx, &a, s, sel, podList.Items, now)
	if derr == nil {
		mDemandAge.WithLabelValues(a.Namespace, a.Name).Set(demand.OldestSampleAge.Seconds())
	}
	var cost *policy.Cost
	var cerr error
	if s.Cfg.CostRequired() {
		cost, cerr = r.costSignal(ctx, &a, s, st, dep.Status.ReadyReplicas, now)
	}
	healthy := derr == nil && cerr == nil
	r.trackHealth(st, healthy, derr, cerr, now)

	// --- fallback ownership transitions (the only place a native HPA is touched) ---
	if handled, res, ferr := r.fallbackFlow(ctx, run, derr, cerr, healthy); handled {
		return res, ferr
	}

	// --- decision ---
	in := policy.Input{
		Now: now, Current: current, Ready: dep.Status.ReadyReplicas, Cost: cost, Infra: summary,
		History: historyFromStatus(st.Recommendations),
	}
	if derr == nil {
		in.Demand = &policy.Demand{Utilization: demand.Value}
		st.Utilization = strconv.FormatFloat(demand.Value*100, 'f', 1, 64)
	}
	if st.LastScaleTime != nil {
		fixFutureTime(&st.LastScaleTime, now)
		in.LastScale = st.LastScaleTime.Time
	}
	dec := policy.Evaluate(s.Cfg, in)
	run.recordDecision(dec, cost)
	st.Recommendations = historyToStatus(dec.History)

	// --- act ---
	scaleErr := run.apply(ctx, dec, current)

	// --- conditions ---
	run.setHealthConditions(derr, cerr, dec, scaleErr)
	if err := r.persist(ctx, &a, st); err != nil {
		return ctrl.Result{}, err
	}
	if scaleErr != nil {
		lg.Info("scale write failed", "error", scaleErr.Error())
	}
	return ctrl.Result{RequeueAfter: r.requeueDelay(s.Interval, st.Failure.ConsecutiveFailures)}, nil
}

// runState bundles per-reconcile data.
type runState struct {
	fallbackUnavailable bool
	r                   *KubeCostAutoscalerReconciler
	a                   *v1alpha1.KubeCostAutoscaler
	st                  *v1alpha1.KubeCostAutoscalerStatus
	s                   Settings
	now                 time.Time
}

// cond sets a condition and emits an event only on a reason transition, so a
// steady state never spams events.
func (rs *runState) cond(t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	old := meta.FindStatusCondition(rs.st.Conditions, t)
	changed := old == nil || old.Reason != reason || old.Status != status
	meta.SetStatusCondition(&rs.st.Conditions, metav1.Condition{
		Type: t, Status: status, Reason: reason, Message: truncate(msg, 512), ObservedGeneration: rs.a.Generation,
	})
	if changed && rs.r.Recorder != nil {
		typ := corev1.EventTypeNormal
		if (t == v1alpha1.ConditionReady && !ok) || t == v1alpha1.ConditionDegraded && ok {
			typ = corev1.EventTypeWarning
		}
		rs.r.Recorder.Event(rs.a, typ, reason, fmt.Sprintf("%s=%s: %s", t, status, truncate(msg, 256)))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (r *KubeCostAutoscalerReconciler) nodeLookup(ctx context.Context) infra.NodeLookup {
	return func(name string) (*corev1.Node, bool) {
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return nil, false
		}
		return &n, true
	}
}

func (r *KubeCostAutoscalerReconciler) demand(ctx context.Context, a *v1alpha1.KubeCostAutoscaler, s Settings, sel labels.Selector, pods []corev1.Pod, now time.Time) (resmetrics.Utilization, error) {
	if r.Resources == nil {
		return resmetrics.Utilization{}, resmetrics.ErrUnavailable
	}
	return r.Resources.Utilization(ctx, a.Namespace, sel, pods, s.Resources, now, s.MaxMetricAge)
}

// costSignal fetches, validates and caches the KubeCost sample. A nil Cost with
// a non-nil error means the signal is untrusted.
func (r *KubeCostAutoscalerReconciler) costSignal(ctx context.Context, a *v1alpha1.KubeCostAutoscaler, s Settings, st *v1alpha1.KubeCostAutoscalerStatus, ready int32, now time.Time) (*policy.Cost, error) {
	key := client.ObjectKeyFromObject(a)
	sample, err := r.fetchCost(ctx, key, kubecost.Query{
		Namespace: a.Namespace, Controller: a.Spec.TargetRef.Name, ControllerKind: "deployment", Cluster: s.Cluster, Window: s.CostWindow,
	}, now)
	if err == nil {
		err = sample.Validate(now, s.MaxMetricAge, 0.5)
	}
	if err != nil {
		mAPIErrors.WithLabelValues(string(classOrUnknown(err))).Inc()
		st.Cost = nil
		return nil, err
	}
	mAge.WithLabelValues(a.Namespace, a.Name).Set(sample.Age(now).Seconds())
	mHourly.WithLabelValues(a.Namespace, a.Name).Set(sample.HourlyCost.Float())
	// Stable only if neither this controller nor anyone else changed the replica count inside the window.
	fixFutureTime(&st.LastScaleTime, now)
	fixFutureTime(&st.LastReplicaChangeTime, now)
	stable := (st.LastScaleTime == nil || now.Sub(st.LastScaleTime.Time) >= s.CostWindow) &&
		(st.LastReplicaChangeTime == nil || now.Sub(st.LastReplicaChangeTime.Time) >= s.CostWindow)
	st.Cost = &v1alpha1.CostStatus{HourlyUSD: sample.HourlyCost.String(), SampleTime: metav1.NewTime(sample.WindowEnd), Stable: stable}
	return &policy.Cost{Hourly: sample.HourlyCost, Stable: stable}, nil
}

// fixFutureTime replaces a stabilisation timestamp that lies after now (clock skew
// or an edited status) with now, once, and the caller persists it. Left alone, the
// elapsed time would stay negative and the cooldown or stability window never end.
func fixFutureTime(t **metav1.Time, now time.Time) {
	if *t != nil && (*t).Time.After(now) {
		nt := metav1.NewTime(now)
		*t = &nt
	}
}

// costReason maps an error class to a CamelCase condition reason.
func costReason(c kubecost.Class) string {
	switch c {
	case kubecost.ClassTransient:
		return "CostTransient"
	case kubecost.ClassAuth:
		return "CostAuth"
	case kubecost.ClassClient:
		return "CostClientError"
	case kubecost.ClassMalformed:
		return "CostMalformed"
	case kubecost.ClassNoData:
		return "CostNoData"
	case kubecost.ClassAmbiguous:
		return "CostAmbiguous"
	case kubecost.ClassStale:
		return "CostStale"
	}
	return "CostUnavailable"
}

func classOrUnknown(err error) kubecost.Class {
	if c := kubecost.ClassOf(err); c != "" {
		return c
	}
	return kubecost.ClassTransient
}

func (r *KubeCostAutoscalerReconciler) fetchCost(ctx context.Context, key types.NamespacedName, q kubecost.Query, now time.Time) (kubecost.Sample, error) {
	r.mu.Lock()
	if c, ok := r.costCache[key]; ok && now.Sub(c.at) < costCacheTTL {
		r.mu.Unlock()
		return c.sample, c.err
	}
	r.mu.Unlock()
	if r.Cost == nil {
		return kubecost.Sample{}, &kubecost.Error{Class: kubecost.ClassClient, Err: errors.New("KubeCost is not configured")}
	}
	began := time.Now()
	sample, err := r.Cost.Allocation(ctx, q)
	outcome := "ok"
	if err != nil {
		outcome = string(classOrUnknown(err))
	}
	mCostRequestSeconds.WithLabelValues(outcome).Observe(time.Since(began).Seconds())
	r.mu.Lock()
	if r.costCache == nil {
		r.costCache = map[types.NamespacedName]cachedCost{}
	}
	r.costCache[key] = cachedCost{sample: sample, err: err, at: now}
	r.mu.Unlock()
	return sample, err
}

func (r *KubeCostAutoscalerReconciler) forgetCost(key types.NamespacedName) {
	r.mu.Lock()
	delete(r.costCache, key)
	r.mu.Unlock()
}

// trackHealth maintains the restart-safe failure bookkeeping in status.
func (r *KubeCostAutoscalerReconciler) trackHealth(st *v1alpha1.KubeCostAutoscalerStatus, healthy bool, derr, cerr error, now time.Time) {
	t := metav1.NewTime(now)
	if healthy {
		if st.Failure.FirstFailureTime != nil && st.Failure.ConsecutiveFailures > 0 {
			mRecoverySeconds.Observe(now.Sub(st.Failure.FirstFailureTime.Time).Seconds())
		}
		if st.Failure.ConsecutiveFailures > 0 || st.Failure.HealthySince == nil {
			st.Failure.HealthySince = &t
		}
		st.Failure.ConsecutiveFailures, st.Failure.FirstFailureTime, st.Failure.LastErrorClass = 0, nil, ""
		st.Failure.LastHealthyTime = &t
		return
	}
	if st.Failure.ConsecutiveFailures == 0 || st.Failure.FirstFailureTime == nil {
		st.Failure.FirstFailureTime = &t
	}
	st.Failure.HealthySince = nil
	st.Failure.ConsecutiveFailures++
	switch {
	case cerr != nil:
		st.Failure.LastErrorClass = "kubecost_" + string(classOrUnknown(cerr))
	case derr != nil:
		st.Failure.LastErrorClass = "resource_metrics"
	}
}

// fallbackFlow implements the Freeze / ResourceMetricsFallback state machine.
// It returns handled=true when the fallback HPA owns scaling this cycle (no
// replica writes by this controller).
func (r *KubeCostAutoscalerReconciler) fallbackFlow(ctx context.Context, rs *runState, derr, cerr error, healthy bool) (bool, ctrl.Result, error) {
	a, st, s, now := rs.a, rs.st, rs.s, rs.now
	if s.Mode != v1alpha1.FailureModeResourceMetricsFallback && !st.Fallback.Active {
		return false, ctrl.Result{}, nil
	}
	costOutage := cerr != nil && derr == nil
	persistAndRequeue := func() (bool, ctrl.Result, error) {
		return true, ctrl.Result{RequeueAfter: r.requeueDelay(s.Interval, st.Failure.ConsecutiveFailures)}, r.persist(ctx, a, st)
	}

	if st.Fallback.Active {
		// Recovery: signals must stay valid for RecoveryDelay before control returns;
		// switching the policy back to Freeze hands control back immediately.
		modeChanged := s.Mode != v1alpha1.FailureModeResourceMetricsFallback
		if modeChanged || (healthy && st.Failure.HealthySince != nil && now.Sub(st.Failure.HealthySince.Time) >= s.RecoveryDelay) {
			if !s.DryRun {
				if _, err := r.Fallback.Remove(ctx, a); err != nil {
					rs.cond(v1alpha1.ConditionDegraded, true, "FallbackRemovalFailed", "could not remove the fallback HPA; will retry")
					_, res, perr := persistAndRequeue()
					return true, res, errors.Join(err, perr)
				}
			}
			st.Fallback = v1alpha1.FallbackStatus{}
			mFallback.WithLabelValues(a.Namespace, a.Name).Set(0)
			rs.cond(v1alpha1.ConditionDegraded, false, "Recovered", "KubeCost data is valid again; the controller resumed")
			return false, ctrl.Result{}, nil
		}
		if !s.DryRun {
			if _, err := r.Fallback.Ensure(ctx, a, s.FallbackCPU); err != nil {
				rs.cond(v1alpha1.ConditionDegraded, true, "FallbackEnsureFailed", err.Error())
				_, res, perr := persistAndRequeue()
				return true, res, errors.Join(err, perr)
			}
		}
		rs.holdForFallback("fallback HPA owns scaling until KubeCost data is valid for "+s.RecoveryDelay.String(), healthy)
		return persistAndRequeue()
	}

	// Activation requires a *cost* outage (the HPA fallback needs working resource metrics)
	// that has lasted FallbackActivationDelay.
	if costOutage && st.Failure.FirstFailureTime != nil && now.Sub(st.Failure.FirstFailureTime.Time) >= s.ActivationDelay {
		if err := r.resourceMetricsOK(ctx); err != nil {
			rs.fallbackUnavailable = true
			return false, ctrl.Result{}, nil // engine runs with Freeze semantics
		}
		if !s.DryRun {
			if _, err := r.Fallback.Ensure(ctx, a, s.FallbackCPU); err != nil {
				rs.cond(v1alpha1.ConditionDegraded, true, "FallbackEnsureFailed", err.Error())
				_, res, perr := persistAndRequeue()
				return true, res, errors.Join(err, perr)
			}
			st.Fallback = v1alpha1.FallbackStatus{Active: true, Since: &metav1.Time{Time: now}, HPAName: ownership.HPAName(a.Name)}
			mFallback.WithLabelValues(a.Namespace, a.Name).Set(1)
		}
		rs.holdForFallback("KubeCost data invalid for "+s.ActivationDelay.String()+": scaling delegated to the native CPU HPA", false)
		return persistAndRequeue()
	}
	return false, ctrl.Result{}, nil
}

func (r *KubeCostAutoscalerReconciler) resourceMetricsOK(ctx context.Context) error {
	if r.Resources == nil {
		return resmetrics.ErrUnavailable
	}
	return r.Resources.Available(ctx)
}

func (rs *runState) holdForFallback(msg string, metricsOK bool) {
	st := rs.st
	st.ScalingOwner = v1alpha1.OwnerFallbackHPA
	st.LastDecision = &v1alpha1.DecisionStatus{
		Type: string(policy.Fallback), Reason: "FallbackActive", Message: msg, ProposedReplicas: st.CurrentReplicas,
		CandidateReplicas: st.CurrentReplicas, DryRun: rs.s.DryRun, Time: metav1.NewTime(rs.now),
	}
	mDecisions.WithLabelValues(string(policy.Fallback), rs.a.Namespace, rs.a.Name).Inc()
	rs.cond(v1alpha1.ConditionDegraded, true, "FallbackActive", msg)
	rs.cond(v1alpha1.ConditionScalingActive, false, "FallbackActive", "the fallback HPA owns the replicas; this controller does not write them")
	if metricsOK {
		rs.cond(v1alpha1.ConditionMetricsAvailable, true, "Fresh", "cost and demand signals are valid (waiting out the recovery delay)")
	} else {
		rs.cond(v1alpha1.ConditionMetricsAvailable, false, "CostUnavailable", "KubeCost data is invalid")
	}
	rs.cond(v1alpha1.ConditionReady, true, "Reconciled", "running in fallback mode")
	if rs.s.DryRun {
		rs.st.LastDecision.Message += " (dry-run: no HPA created)"
	}
	t := metav1.NewTime(rs.now)
	st.LastSuccessfulReconcileTime = &t
	if st.Fallback.Active {
		mFallback.WithLabelValues(rs.a.Namespace, rs.a.Name).Set(1)
	}
}

func (rs *runState) recordDecision(dec policy.Decision, cost *policy.Cost) {
	st := rs.st
	st.DesiredReplicas = dec.Proposed
	st.LastDecision = &v1alpha1.DecisionStatus{
		Type: string(dec.Type), Reason: string(dec.Reason), Message: truncate(dec.Message, 512), ProposedReplicas: dec.Proposed,
		CandidateReplicas: dec.Candidate, CostDriven: dec.CostDriven, DryRun: rs.s.DryRun, Time: metav1.NewTime(rs.now),
	}
	if cost != nil && st.Cost != nil {
		st.Cost.MarginalAddUSD, st.Cost.MarginalRemoveUSD = dec.Details.MarginalAdd.String(), dec.Details.MarginalRemove.String()
		st.Cost.ProjectedUSD = dec.Details.ProjectedHourly.String()
		st.Cost.BudgetExceeded = rs.s.Cfg.MaxHourly > 0 && cost.Hourly > rs.s.Cfg.MaxHourly
		st.Cost.BudgetLimited = dec.Details.BudgetLimited
		mBudget.WithLabelValues(rs.a.Namespace, rs.a.Name).Set(rs.s.Cfg.MaxHourly.Float())
	}
	mDecisions.WithLabelValues(string(dec.Type), rs.a.Namespace, rs.a.Name).Inc()
	mReasons.WithLabelValues(string(dec.Reason), rs.a.Namespace, rs.a.Name).Inc()
	mDesired.WithLabelValues(rs.a.Namespace, rs.a.Name).Set(float64(dec.Proposed))
}

// apply performs the replica write for ScaleUp/ScaleDown decisions.
//
// Crash safety: the intent (lastScaleTime/From/To) is persisted BEFORE the
// write. If the process dies in between, the next reconcile sees a recent
// lastScaleTime and waits out the cooldown instead of acting twice.
func (rs *runState) apply(ctx context.Context, dec policy.Decision, current int32) error {
	st := rs.st
	st.ScalingOwner = v1alpha1.OwnerOperator
	if rs.s.DryRun {
		st.ScalingOwner = v1alpha1.OwnerNone
	}
	if dec.Type != policy.ScaleUp && dec.Type != policy.ScaleDown {
		return nil
	}
	if rs.s.DryRun {
		prefix := "dry-run"
		if rs.s.Paused {
			prefix = "paused"
		}
		st.LastDecision.Message = prefix + ": would scale " + fmt.Sprintf("%d -> %d. ", current, dec.Proposed) + st.LastDecision.Message
		return nil
	}
	dir := "up"
	if dec.Type == policy.ScaleDown {
		dir = "down"
	}
	prevTime, prevFrom, prevTo := st.LastScaleTime, st.LastScaleFrom, st.LastScaleTo
	t := metav1.NewTime(rs.now)
	st.LastScaleTime, st.LastScaleFrom, st.LastScaleTo = &t, current, dec.Proposed
	if err := rs.r.persist(ctx, rs.a, st); err != nil {
		st.LastScaleTime, st.LastScaleFrom, st.LastScaleTo = prevTime, prevFrom, prevTo
		return fmt.Errorf("persisting scale intent: %w", err)
	}
	if err := rs.r.Scaler.Scale(ctx, rs.a.Namespace, rs.a.Spec.TargetRef.Name, current, dec.Proposed); err != nil {
		st.LastScaleTime, st.LastScaleFrom, st.LastScaleTo = prevTime, prevFrom, prevTo
		mActions.WithLabelValues(dir, "error", rs.a.Namespace, rs.a.Name).Inc()
		reason := "ScaleFailed"
		if errors.Is(err, actuator.ErrReplicaDrift) {
			reason = "ReplicaDrift"
		}
		rs.cond(v1alpha1.ConditionScalingActive, false, reason, "could not write replicas: "+truncate(err.Error(), 200))
		return err
	}
	mActions.WithLabelValues(dir, "ok", rs.a.Namespace, rs.a.Name).Inc()
	if rs.r.Recorder != nil {
		rs.r.Recorder.Eventf(rs.a, corev1.EventTypeNormal, string(dec.Reason), "scaled %d -> %d: %s", current, dec.Proposed, truncate(dec.Message, 200))
	}
	st.CurrentReplicas = dec.Proposed
	return nil
}

func (rs *runState) setHealthConditions(derr, cerr error, dec policy.Decision, scaleErr error) {
	st := rs.st
	ok := derr == nil && cerr == nil
	switch {
	case derr != nil:
		rs.cond(v1alpha1.ConditionMetricsAvailable, false, "DemandUnavailable", "resource metrics are unavailable or invalid")
	case cerr != nil:
		rs.cond(v1alpha1.ConditionMetricsAvailable, false, costReason(classOrUnknown(cerr)), "KubeCost data is unavailable or invalid ("+string(classOrUnknown(cerr))+")")
	default:
		rs.cond(v1alpha1.ConditionMetricsAvailable, true, "Fresh", "cost and demand signals are valid")
	}
	switch {
	case ok:
		rs.cond(v1alpha1.ConditionDegraded, false, "Healthy", "all signals are valid")
	case rs.fallbackUnavailable:
		rs.cond(v1alpha1.ConditionDegraded, true, "FallbackUnavailable", "fallback requested but the resource metrics API is unavailable; staying frozen")
	default:
		rs.cond(v1alpha1.ConditionDegraded, true, "Frozen", "signals invalid: replicas are held; only safety scale-up is allowed")
	}
	switch {
	case scaleErr != nil:
		// condition already set by apply()
	case rs.s.Paused:
		rs.cond(v1alpha1.ConditionScalingActive, false, "Paused", "paused by the "+v1alpha1.AnnotationPause+" annotation: decisions are reported but nothing is written")
	case rs.s.DryRun:
		rs.cond(v1alpha1.ConditionScalingActive, false, "DryRun", "dry-run: decisions are reported but not applied")
	default:
		rs.cond(v1alpha1.ConditionScalingActive, true, "Owner", "this controller is the single writer of the target's replicas")
	}
	rs.cond(v1alpha1.ConditionReady, true, "Reconciled", dec.Message)
	t := metav1.NewTime(rs.now)
	st.LastSuccessfulReconcileTime = &t
}

// requeueDelay is the evaluation interval, backed off exponentially (with
// jitter) while signals keep failing so a sick dependency is not hammered.
func (r *KubeCostAutoscalerReconciler) requeueDelay(interval time.Duration, failures int32) time.Duration {
	d := interval
	for i := int32(1); i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	j := rand.Float64
	if r.Jitter != nil {
		j = r.Jitter
	}
	return d + time.Duration(float64(d)*0.1*j())
}

// persist writes the status, retrying on update conflicts. The status is
// owned by this controller, so on conflict the latest object is re-read and
// the computed status is re-applied on top of it.
func (r *KubeCostAutoscalerReconciler) persist(ctx context.Context, a *v1alpha1.KubeCostAutoscaler, st *v1alpha1.KubeCostAutoscalerStatus) error {
	if len(st.Recommendations) > maxRecommendationLen {
		st.Recommendations = st.Recommendations[len(st.Recommendations)-maxRecommendationLen:]
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest v1alpha1.KubeCostAutoscaler
		if err := r.Get(ctx, client.ObjectKeyFromObject(a), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status = *st.DeepCopy()
		return r.Status().Update(ctx, &latest)
	})
}

func historyFromStatus(in []v1alpha1.Recommendation) []policy.Recommendation {
	out := make([]policy.Recommendation, 0, len(in))
	for _, r := range in {
		out = append(out, policy.Recommendation{Time: r.Time.Time, Replicas: r.Replicas})
	}
	return out
}

func historyToStatus(in []policy.Recommendation) []v1alpha1.Recommendation {
	out := make([]v1alpha1.Recommendation, 0, len(in))
	for _, r := range in {
		out = append(out, v1alpha1.Recommendation{Time: metav1.NewTime(r.Time), Replicas: r.Replicas})
	}
	return out
}

func setReplicaMetrics(a *v1alpha1.KubeCostAutoscaler, current int32) {
	mCurrent.WithLabelValues(a.Namespace, a.Name).Set(float64(current))
}

func setNodeMetrics(a *v1alpha1.KubeCostAutoscaler, s infra.Summary) {
	for class, v := range map[string]int32{"spot": s.Spot, "ondemand": s.OnDemand, "unknown": s.Unknown, "pending": s.Pending} {
		mNodes.WithLabelValues(class, a.Namespace, a.Name).Set(float64(v))
	}
}

// SetupWithManager registers the controller, its index and its watches.
func (r *KubeCostAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.KubeCostAutoscaler{}, targetIndex,
		func(o client.Object) []string { return []string{o.(*v1alpha1.KubeCostAutoscaler).Spec.TargetRef.Name} }); err != nil {
		return err
	}
	byDeployment := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var list v1alpha1.KubeCostAutoscalerList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{targetIndex: o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	})
	// Only replica-relevant Deployment changes wake the loop; status churn of
	// the autoscaler itself (generation unchanged) is filtered out below.
	deploymentChanged := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*appsv1.Deployment)
			n, ok2 := e.ObjectNew.(*appsv1.Deployment)
			if !ok1 || !ok2 {
				return true
			}
			return !int32PtrEq(o.Spec.Replicas, n.Spec.Replicas) || o.Status.ReadyReplicas != n.Status.ReadyReplicas || o.Generation != n.Generation
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("kubecostautoscaler").
		For(&v1alpha1.KubeCostAutoscaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(&appsv1.Deployment{}, byDeployment, builder.WithPredicates(deploymentChanged)).
		Complete(r)
}

func int32PtrEq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
