// Package controller contains the KubeCostHPA and Spot node reconcilers.
package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/config"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

// Finalizer makes the operator restore the original pod resources before a
// KubeCostHPA disappears.
const Finalizer = "autoscaling.kubecost.io/restore-resources"

// restoreDeadline bounds how long a deletion may be held by the finalizer: the
// restore is best effort and must never block deleting the resource.
const restoreDeadline = 2 * time.Minute

const (
	condReady   = "Ready"
	condMetrics = "MetricsAvailable"

	metricsTTL      = 30 * time.Second // fresh-cache window: avoids hammering Prometheus
	metricsStaleTTL = 15 * time.Minute // fallback window when Prometheus is down
)

// KubeCostHPAReconciler evaluates unit economics for a Deployment and resizes
// its pods in place.
type KubeCostHPAReconciler struct {
	client.Client
	Resizer  *actuator.Resizer
	Metrics  metrics.Source
	Spend    metrics.SpendSource // optional
	Engine   engine.Evaluator
	Store    cache.Store
	Recorder record.EventRecorder
	Defaults config.Defaults
	Now      func() time.Time
}

// storedDecision is the temporary scaling state kept in the cache.
type storedDecision struct {
	Action string    `json:"action"`
	CPU    int64     `json:"cpu"`
	At     time.Time `json:"at"`
}

// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecosthpas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecosthpas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.kubecost.io,resources=kubecosthpas/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=patch;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *KubeCostHPAReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile implements the evaluation loop.
func (r *KubeCostHPAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	var hpa v1alpha1.KubeCostHPA
	if err := r.Get(ctx, req.NamespacedName, &hpa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hpa.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &hpa)
	}
	if controllerutil.AddFinalizer(&hpa, Finalizer) {
		if err := r.Update(ctx, &hpa); err != nil {
			return ctrl.Result{}, err
		}
	}
	interval := hpa.Spec.Interval.Duration
	if interval <= 0 {
		interval = time.Minute
	}
	requeue := ctrl.Result{RequeueAfter: interval}
	key := fmt.Sprintf("kchpa:%s/%s", hpa.Namespace, hpa.Name)

	fail := func(reason, msg string) (ctrl.Result, error) {
		meta.SetStatusCondition(&hpa.Status.Conditions, metav1.Condition{Type: condReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: hpa.Generation})
		hpa.Status.ObservedGeneration = hpa.Generation
		if err := r.Status().Update(ctx, &hpa); err != nil {
			return ctrl.Result{}, err
		}
		return requeue, nil
	}

	// --- target workload ---
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: hpa.Namespace, Name: hpa.Spec.TargetRef.Name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("TargetNotFound", fmt.Sprintf("deployment %q not found", hpa.Spec.TargetRef.Name))
		}
		return ctrl.Result{}, err
	}
	cname, tmpl := pickContainer(&dep, hpa.Spec.TargetRef.ContainerName)
	if tmpl == nil {
		return fail("ContainerNotFound", "target container not found in deployment")
	}
	baseCPU := tmpl.Resources.Requests.Cpu().MilliValue()
	if baseCPU == 0 {
		return fail("NoCPURequest", "target container has no CPU request")
	}
	prices, err := r.prices(&hpa)
	if err != nil {
		return fail("InvalidSpec", err.Error())
	}
	minCPU, maxCPU := hpa.Spec.CPU.Min.MilliValue(), hpa.Spec.CPU.Max.MilliValue()
	// Typed clients serialize zero-valued structs, which bypasses CRD defaulting: apply the same defaults here.
	if minCPU == 0 {
		minCPU = 100
	}
	if maxCPU == 0 {
		maxCPU = 4000
	}
	if hpa.Spec.Cooldown.Duration == 0 {
		hpa.Spec.Cooldown.Duration = 5 * time.Minute
	}
	if minCPU <= 0 || maxCPU < minCPU {
		return fail("InvalidSpec", "cpu.min must be > 0 and <= cpu.max")
	}

	// --- pods ---
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return fail("InvalidSelector", err.Error())
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(hpa.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	pods := runningPods(podList.Items)

	desired := hpa.Status.DesiredCPUMillicores
	if desired == 0 {
		desired = baseCPU
	}
	baseMemMiB := tmpl.Resources.Requests.Memory().Value() >> 20
	desiredMem := hpa.Status.DesiredMemoryMiB
	if desiredMem == 0 {
		desiredMem = baseMemMiB
	}
	curReplicas := int32(1)
	if dep.Spec.Replicas != nil {
		curReplicas = *dep.Spec.Replicas
	}
	if hpa.Spec.Memory != nil && baseMemMiB == 0 {
		return fail("NoMemoryRequest", "memory resizing requires a memory request on the target container")
	}
	if rs := hpa.Spec.Replicas; rs != nil && (rs.Min < 1 || rs.Max < rs.Min) {
		return fail("InvalidSpec", "replicas: require 1 <= min <= max")
	}

	// --- converge existing/new pods to the desired values ---
	if !hpa.Spec.DryRun && (hpa.Status.DesiredCPUMillicores != 0 || hpa.Status.DesiredMemoryMiB != 0) {
		r.converge(ctx, &hpa, pods, cname, actuator.Target{CPUMillis: desired, MemoryBytes: memBytes(hpa.Spec.Memory, desiredMem)})
	}

	// --- metrics (cache first, stale fallback) ---
	wl := metrics.Workload{Namespace: hpa.Namespace, Name: dep.Name}
	if m := hpa.Spec.Metrics; m != nil {
		wl.Overrides = &metrics.Queries{RPS: m.RPSQuery, P99Ms: m.P99Query, CPUUsage: m.CPUUsageQuery, MemUsage: m.MemUsageQuery}
	}
	snap, err := r.snapshot(ctx, key, wl)
	if err != nil {
		meta.SetStatusCondition(&hpa.Status.Conditions, metav1.Condition{Type: condMetrics, Status: metav1.ConditionFalse, Reason: "Unavailable", Message: err.Error(), ObservedGeneration: hpa.Generation})
		return fail("NoMetrics", err.Error())
	}
	meta.SetStatusCondition(&hpa.Status.Conditions, metav1.Condition{Type: condMetrics, Status: metav1.ConditionTrue, Reason: "Fresh", ObservedGeneration: hpa.Generation})

	nPods := int64(len(pods))
	perPodRPS := snap.RPS
	if nPods > 0 {
		perPodRPS = snap.RPS / float64(nPods)
	}

	// --- engine request ---
	ereq := engine.Request{
		CPUMillicores: desired, P99Ms: snap.P99Ms, SLOP99Ms: float64(hpa.Spec.SLO.P99LatencyMs),
		RPS: perPodRPS, MinMillicores: minCPU, MaxMillicores: maxCPU,
		MemoryGiB:           float64(tmpl.Resources.Requests.Memory().Value()) / (1 << 30),
		CPUPricePerCoreHour: prices.cpu, MemPricePerGiBHour: prices.mem, MaxCostPerRequest: prices.maxCPR,
	}
	if snap.CPUUsageMillicores > 0 {
		u := snap.CPUUsageMillicores
		ereq.CPUUsageMillicores = &u
	}
	if hpa.Spec.Budget != nil && r.Spend != nil {
		limit, perr := strconv.ParseFloat(hpa.Spec.Budget.MonthlyLimitUSD, 64)
		if perr != nil {
			return fail("InvalidSpec", "budget.monthlyLimitUSD: "+perr.Error())
		}
		if spent, serr := r.Spend.MonthlySpend(ctx, hpa.Namespace); serr != nil {
			if hpa.Spec.Budget.OnSpendUnavailable == "FreezeGrowth" {
				lg.Info("spend unavailable, freezing growth (onSpendUnavailable=FreezeGrowth)", "error", serr.Error())
				ereq.Budget = &engine.Budget{MonthlyLimit: limit, Spent: limit}
			} else {
				lg.Info("spend unavailable, budget not enforced this cycle", "error", serr.Error())
			}
		} else {
			ereq.Budget = &engine.Budget{MonthlyLimit: limit, Spent: spent}
		}
	}
	if m := hpa.Spec.Memory; m != nil && snap.MemUsageMiB > 0 {
		minMiB, maxMiB := m.Min.Value()>>20, m.Max.Value()>>20
		if minMiB == 0 {
			minMiB = 64
		}
		if maxMiB == 0 {
			maxMiB = 16384
		}
		ereq.Memory = &engine.MemoryReq{RequestMiB: desiredMem, UsageMiB: snap.MemUsageMiB, MinMiB: minMiB, MaxMiB: maxMiB}
	}
	if rs := hpa.Spec.Replicas; rs != nil {
		ereq.Replicas = &engine.ReplicasReq{Current: curReplicas, Min: rs.Min, Max: rs.Max}
	}
	if hpa.Spec.Spot.Enabled {
		ereq.Spot = &engine.Spot{
			FaultTolerant: hpa.Spec.Spot.FaultTolerant, Replicas: int(nPods),
			HasPDB: r.hasPDB(ctx, &dep), MinReplicas: int(hpa.Spec.Spot.MinReplicas),
		}
	}
	dec, err := r.Engine.Evaluate(ctx, ereq)
	if err != nil {
		r.event(&hpa, corev1.EventTypeWarning, "EngineError", err.Error())
		return fail("EngineError", err.Error())
	}

	// --- status bookkeeping ---
	now := metav1.NewTime(r.now())
	var prevEval *metav1.Time
	if hpa.Status.LastEvaluationTime != nil {
		t := *hpa.Status.LastEvaluationTime
		prevEval = &t
	}
	hpa.Status.LastEvaluationTime = &now
	hpa.Status.RPS = strconv.FormatFloat(snap.RPS, 'f', 2, 64)
	hpa.Status.P99LatencyMs = strconv.FormatFloat(snap.P99Ms, 'f', 1, 64)
	if dec.CostPerRequest != nil {
		hpa.Status.CostPerRequest = strconv.FormatFloat(*dec.CostPerRequest, 'g', 4, 64)
	}
	hpa.Status.BudgetFrozen = dec.BudgetFrozen
	hpa.Status.SpotEligible = dec.SpotEligible != nil && *dec.SpotEligible
	hpa.Status.LastAction, hpa.Status.LastReason = dec.Action, dec.Reason
	hpa.Status.ObservedGeneration = hpa.Generation

	// --- act ---
	newMem := desiredMem
	if dec.Memory != nil && dec.Memory.Action != "hold" {
		newMem = dec.Memory.MiB
	}
	newReplicas := curReplicas
	if dec.Replicas != nil {
		newReplicas = dec.Replicas.Target
	}
	cpuChange := dec.Action != "hold" && dec.CPUMillicores != desired
	memChange := newMem != desiredMem
	repChange := newReplicas != curReplicas
	if cpuChange || memChange || repChange {
		var parts []string
		if cpuChange {
			parts = append(parts, fmt.Sprintf("cpu %dm->%dm (%s)", desired, dec.CPUMillicores, dec.Reason))
		}
		if memChange {
			parts = append(parts, fmt.Sprintf("memory %dMi->%dMi (%s)", desiredMem, newMem, dec.Memory.Reason))
		}
		if repChange {
			parts = append(parts, fmt.Sprintf("replicas %d->%d (%s)", curReplicas, newReplicas, dec.Replicas.Reason))
		}
		summary := strings.Join(parts, "; ")
		isGrow := (cpuChange && dec.Action == "grow") || (memChange && dec.Memory.Action == "grow") || newReplicas > curReplicas
		switch {
		case hpa.Spec.DryRun:
			hpa.Status.LastReason = "dry-run: would " + summary
		case r.inCooldown(ctx, key, &hpa, isGrow):
			hpa.Status.LastReason = "suppressed by cooldown: " + summary
		default:
			if cpuChange {
				desired = dec.CPUMillicores
				hpa.Status.DesiredCPUMillicores = desired
				cResizes.WithLabelValues(hpa.Namespace, hpa.Name, dec.Action).Inc()
			}
			if memChange {
				desiredMem = newMem
				hpa.Status.DesiredMemoryMiB = desiredMem
				cResizes.WithLabelValues(hpa.Namespace, hpa.Name, "memory-"+dec.Memory.Action).Inc()
			}
			if repChange {
				if err := r.Resizer.SetReplicas(ctx, hpa.Namespace, dep.Name, newReplicas); err != nil {
					r.event(&hpa, corev1.EventTypeWarning, "ScaleFailed", err.Error())
					lg.Info("replica scale failed", "error", err.Error())
				} else {
					hpa.Status.DesiredReplicas = newReplicas
					cResizes.WithLabelValues(hpa.Namespace, hpa.Name, "replicas").Inc()
				}
			}
			hpa.Status.LastResizeTime = &now
			hpa.Status.LastResizeAction = "shrink"
			if isGrow {
				hpa.Status.LastResizeAction = "grow"
			}
			if cpuChange || memChange {
				r.converge(ctx, &hpa, pods, cname, actuator.Target{CPUMillis: desired, MemoryBytes: memBytes(hpa.Spec.Memory, desiredMem)})
			}
			if serr := r.Store.Set(ctx, key+":decision", storedDecision{dec.Action, desired, r.now()}, hpa.Spec.Cooldown.Duration); serr != nil {
				lg.Info("cache write failed", "error", serr.Error())
			}
			hpa.Status.LastReason = summary
			r.event(&hpa, corev1.EventTypeNormal, "Scaled", summary)
		}
	}
	exportMetrics(&hpa, nPods, baseCPU, desired, dec, prices.cpu)
	r.accrueSavings(&hpa, prevEval, nPods, baseCPU, desired, prices.cpu)

	meta.SetStatusCondition(&hpa.Status.Conditions, metav1.Condition{Type: condReady, Status: metav1.ConditionTrue, Reason: "Reconciled", Message: hpa.Status.LastReason, ObservedGeneration: hpa.Generation})
	if err := r.Status().Update(ctx, &hpa); err != nil {
		return ctrl.Result{}, err
	}
	return requeue, nil
}

type priceSet struct{ cpu, mem, maxCPR float64 }

func (r *KubeCostHPAReconciler) prices(h *v1alpha1.KubeCostHPA) (priceSet, error) {
	p := priceSet{cpu: r.Defaults.CPUPricePerCoreHour, mem: r.Defaults.MemPricePerGiBHour}
	for _, f := range []struct {
		s    string
		name string
		dst  *float64
	}{
		{h.Spec.UnitEconomics.CPUPricePerCoreHour, "cpuPricePerCoreHour", &p.cpu},
		{h.Spec.UnitEconomics.MemPricePerGiBHour, "memPricePerGiBHour", &p.mem},
		{h.Spec.UnitEconomics.MaxCostPerRequest, "maxCostPerRequest", &p.maxCPR},
	} {
		if f.s == "" {
			continue
		}
		v, err := strconv.ParseFloat(f.s, 64)
		if err != nil || v < 0 {
			return p, fmt.Errorf("unitEconomics.%s: invalid value %q", f.name, f.s)
		}
		*f.dst = v
	}
	return p, nil
}

func (r *KubeCostHPAReconciler) snapshot(ctx context.Context, key string, w metrics.Workload) (metrics.Snapshot, error) {
	var s metrics.Snapshot
	if ok, err := r.Store.Get(ctx, key+":metrics", &s); err == nil && ok {
		return s, nil
	}
	s, err := r.Metrics.Snapshot(ctx, w)
	if err != nil {
		var stale metrics.Snapshot
		if ok, serr := r.Store.Get(ctx, key+":metrics:last", &stale); serr == nil && ok {
			return stale, nil
		}
		return metrics.Snapshot{}, err
	}
	_ = r.Store.Set(ctx, key+":metrics", s, metricsTTL)
	_ = r.Store.Set(ctx, key+":metrics:last", s, metricsStaleTTL)
	return s, nil
}

// growCooldown is the minimum gap between two consecutive growth steps.
const growCooldown = 30 * time.Second

// inCooldown implements an asymmetric anti-flapping policy: reductions wait the
// full cooldown after any resize, but growth (an SLO emergency) is only delayed
// when the previous resize was itself a growth, and then by at most growCooldown.
func (r *KubeCostHPAReconciler) inCooldown(ctx context.Context, key string, h *v1alpha1.KubeCostHPA, grow bool) bool {
	last := h.Status.LastResizeTime
	if grow {
		if last == nil || h.Status.LastResizeAction != "grow" {
			return false
		}
		return r.now().Sub(last.Time) < min(growCooldown, h.Spec.Cooldown.Duration)
	}
	var d storedDecision
	if ok, err := r.Store.Get(ctx, key+":decision", &d); err == nil && ok {
		return true
	}
	return last != nil && r.now().Sub(last.Time) < h.Spec.Cooldown.Duration
}

func (r *KubeCostHPAReconciler) converge(ctx context.Context, h *v1alpha1.KubeCostHPA, pods []corev1.Pod, container string, t actuator.Target) {
	lg := log.FromContext(ctx)
	for i := range pods {
		if _, err := r.Resizer.Resize(ctx, &pods[i], container, t); err != nil {
			lg.Info("pod resize skipped", "pod", pods[i].Name, "reason", err.Error())
			if err == actuator.ErrWouldRestart {
				r.event(h, corev1.EventTypeWarning, "ResizeRefused", pods[i].Name+": "+err.Error())
			}
		}
	}
}

func memBytes(m *v1alpha1.MemorySpec, mib int64) int64 {
	if m == nil {
		return 0 // memory resizing not enabled: leave memory alone
	}
	return mib << 20
}

// accrueSavings adds the estimated CPU savings since the previous evaluation
// (vs. the Deployment template) to the cumulative counter.
func (r *KubeCostHPAReconciler) accrueSavings(h *v1alpha1.KubeCostHPA, prev *metav1.Time, pods, baseCPU, desired int64, cpuPrice float64) {
	if prev == nil {
		return
	}
	rate := float64(baseCPU-desired) / 1000 * float64(pods) * cpuPrice // USD/hour, negative when above template
	hours := r.now().Sub(prev.Time).Hours()
	if hours <= 0 || hours > 1 || rate <= 0 { // ignore gaps (restarts) and negative "savings"
		return
	}
	cSavings.WithLabelValues(h.Namespace, h.Name).Add(rate * hours)
}

func (r *KubeCostHPAReconciler) hasPDB(ctx context.Context, dep *appsv1.Deployment) bool {
	var l policyv1.PodDisruptionBudgetList
	if err := r.List(ctx, &l, client.InNamespace(dep.Namespace)); err != nil {
		return false
	}
	podLabels := labels.Set(dep.Spec.Template.Labels)
	for _, p := range l.Items {
		if p.Spec.Selector == nil {
			continue
		}
		if s, err := metav1.LabelSelectorAsSelector(p.Spec.Selector); err == nil && !s.Empty() && s.Matches(podLabels) {
			return true
		}
	}
	return false
}

func (r *KubeCostHPAReconciler) event(h *v1alpha1.KubeCostHPA, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(h, typ, reason, msg)
	}
}

func pickContainer(d *appsv1.Deployment, name string) (string, *corev1.Container) {
	cs := d.Spec.Template.Spec.Containers
	for i := range cs {
		if name == "" || cs[i].Name == name {
			return cs[i].Name, &cs[i]
		}
	}
	return "", nil
}

func runningPods(in []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range in {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetupWithManager registers the controller.
func (r *KubeCostHPAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.KubeCostHPA{}).Named("kubecosthpa").Complete(r)
}

// finalize returns the pods to the resources declared in the Deployment
// template (the state before the operator touched them) and releases the CR.
// Replicas are left as they are: scaling in/out is not reverted.
func (r *KubeCostHPAReconciler) finalize(ctx context.Context, hpa *v1alpha1.KubeCostHPA) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(hpa, Finalizer) {
		return ctrl.Result{}, nil
	}
	release := func() (ctrl.Result, error) {
		controllerutil.RemoveFinalizer(hpa, Finalizer)
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, hpa))
	}
	touched := hpa.Status.DesiredCPUMillicores != 0 || hpa.Status.DesiredMemoryMiB != 0
	if !touched || hpa.Spec.DryRun {
		return release()
	}
	lg := log.FromContext(ctx)
	if r.now().Sub(hpa.DeletionTimestamp.Time) > restoreDeadline {
		lg.Info("giving up restoring resources, releasing the resource anyway")
		return release()
	}
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: hpa.Namespace, Name: hpa.Spec.TargetRef.Name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return release()
		}
		return ctrl.Result{}, err
	}
	cname, tmpl := pickContainer(&dep, hpa.Spec.TargetRef.ContainerName)
	if tmpl == nil {
		return release()
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return release()
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(hpa.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	target := actuator.Target{CPUMillis: tmpl.Resources.Requests.Cpu().MilliValue()}
	if hpa.Spec.Memory != nil {
		target.MemoryBytes = tmpl.Resources.Requests.Memory().Value()
	}
	pending := false
	pods := runningPods(podList.Items)
	for i := range pods {
		if _, err := r.Resizer.Resize(ctx, &pods[i], cname, target); err != nil {
			lg.Info("restore skipped", "pod", pods[i].Name, "reason", err.Error())
			pending = pending || errors.Is(err, actuator.ErrResizeInFlight)
		}
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	r.event(hpa, corev1.EventTypeNormal, "Restored", "pod resources restored to the Deployment template")
	return release()
}
