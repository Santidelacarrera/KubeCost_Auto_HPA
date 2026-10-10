// Package controller contains the KubeCostHPA and Spot node reconcilers.
package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/actuator"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cache"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/config"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/metrics"
)

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
		if errors.IsNotFound(err) {
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

	// --- converge existing/new pods to the desired value ---
	if !hpa.Spec.DryRun && hpa.Status.DesiredCPUMillicores != 0 {
		r.converge(ctx, &hpa, pods, cname, desired)
	}

	// --- metrics (cache first, stale fallback) ---
	snap, err := r.snapshot(ctx, key, metrics.Workload{Namespace: hpa.Namespace, Name: dep.Name})
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
		// Fail-open: if spend is unknown we keep serving the SLO instead of freezing.
		if spent, serr := r.Spend.MonthlySpend(ctx, hpa.Namespace); serr != nil {
			lg.Info("spend unavailable, budget not enforced this cycle", "error", serr.Error())
		} else {
			ereq.Budget = &engine.Budget{MonthlyLimit: limit, Spent: spent}
		}
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
	exportMetrics(&hpa, nPods, baseCPU, desired, dec, prices.cpu)

	// --- act ---
	if dec.Action != "hold" && dec.CPUMillicores != desired && !r.inCooldown(ctx, key, &hpa) {
		if hpa.Spec.DryRun {
			hpa.Status.LastReason = "dry-run: would " + dec.Action + " to " + strconv.FormatInt(dec.CPUMillicores, 10) + "m (" + dec.Reason + ")"
		} else {
			hpa.Status.DesiredCPUMillicores = dec.CPUMillicores
			hpa.Status.LastResizeTime = &now
			exportMetrics(&hpa, nPods, baseCPU, dec.CPUMillicores, dec, prices.cpu)
			r.converge(ctx, &hpa, pods, cname, dec.CPUMillicores)
			cResizes.WithLabelValues(hpa.Namespace, hpa.Name, dec.Action).Inc()
			if serr := r.Store.Set(ctx, key+":decision", storedDecision{dec.Action, dec.CPUMillicores, r.now()}, hpa.Spec.Cooldown.Duration); serr != nil {
				lg.Info("cache write failed", "error", serr.Error())
			}
			r.event(&hpa, corev1.EventTypeNormal, "Resized", fmt.Sprintf("%s %dm -> %dm: %s", dec.Action, desired, dec.CPUMillicores, dec.Reason))
		}
	} else if dec.Action != "hold" {
		hpa.Status.LastReason = dec.Reason + " (suppressed: cooldown or no change)"
	}

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

func (r *KubeCostHPAReconciler) inCooldown(ctx context.Context, key string, h *v1alpha1.KubeCostHPA) bool {
	var d storedDecision
	if ok, err := r.Store.Get(ctx, key+":decision", &d); err == nil && ok {
		return true
	}
	if t := h.Status.LastResizeTime; t != nil && r.now().Sub(t.Time) < h.Spec.Cooldown.Duration {
		return true
	}
	return false
}

func (r *KubeCostHPAReconciler) converge(ctx context.Context, h *v1alpha1.KubeCostHPA, pods []corev1.Pod, container string, cpu int64) {
	lg := log.FromContext(ctx)
	for i := range pods {
		if _, err := r.Resizer.ResizeCPU(ctx, &pods[i], container, cpu); err != nil {
			lg.Info("pod resize skipped", "pod", pods[i].Name, "reason", err.Error())
			if err == actuator.ErrWouldRestart {
				r.event(h, corev1.EventTypeWarning, "ResizeRefused", pods[i].Name+": "+err.Error())
			}
		}
	}
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
