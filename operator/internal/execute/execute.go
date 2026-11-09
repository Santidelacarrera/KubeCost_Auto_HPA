// Package execute applies an authorised recommendation to a Deployment, watches the result and rolls it back
// when it degrades. It only ever acts after safety.Evaluate allows it, defaults to a dry run, records the previous
// state on the object itself (so a restart or a crash can still roll back) and writes an audit trail.
//
// Scope (ADR-0002): Deployments, container requests (and limits only when they would fall below the request).
// It is a library driven by cmd/rightsize, not a controller: nothing here runs unless a human invokes it.
package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/safety"
)

// Annotations written on the Deployment (not the pod template, so they never cause a rollout).
const (
	AnnApproved   = "autoscaling.kubecost.io/approved-recommendation"
	AnnApplied    = "autoscaling.kubecost.io/applied-recommendation"
	AnnPrevious   = "autoscaling.kubecost.io/previous-resources"
	AnnRolledBack = "autoscaling.kubecost.io/rolled-back-recommendation"
	AnnAppliedAt  = "autoscaling.kubecost.io/last-applied-at"
)

var errNoSavedState = errors.New("no saved state")

var vpaGVR = schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}

// Executor holds the dependencies. Dyn may be nil, in which case VPA presence cannot be established and the policy denies (fail closed).
type Executor struct {
	KC    kubernetes.Interface
	Dyn   dynamic.Interface
	Audit Sink
	Actor string
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (e *Executor) record(typ string, rec recommend.Recommendation, details map[string]any) {
	if e.Audit == nil {
		return
	}
	e.Audit.Record(Event{Time: e.now().UTC(), Type: typ, Actor: e.Actor, RecommendationID: rec.ID,
		Target: rec.Namespace + "/" + rec.Kind + "/" + rec.Workload, Details: details})
}

// ContainerResources are the values written or restored. Zero means "not set".
type ContainerResources struct {
	CPURequest string `json:"cpuRequest,omitempty"`
	CPULimit   string `json:"cpuLimit,omitempty"`
	MemRequest string `json:"memRequest,omitempty"`
	MemLimit   string `json:"memLimit,omitempty"`
}

// Previous is the state saved before a change.
type Previous struct {
	RecommendationID string                        `json:"recommendationId"`
	Actor            string                        `json:"actor"`
	At               time.Time                     `json:"at"`
	Containers       map[string]ContainerResources `json:"containers"`
	Replicas         int32                         `json:"replicas"`
}

func capture(c corev1.Container) ContainerResources {
	q := func(l corev1.ResourceList, n corev1.ResourceName) string {
		if v, ok := l[n]; ok {
			return v.String()
		}
		return ""
	}
	return ContainerResources{
		CPURequest: q(c.Resources.Requests, corev1.ResourceCPU), CPULimit: q(c.Resources.Limits, corev1.ResourceCPU),
		MemRequest: q(c.Resources.Requests, corev1.ResourceMemory), MemLimit: q(c.Resources.Limits, corev1.ResourceMemory),
	}
}

func setQty(l *corev1.ResourceList, n corev1.ResourceName, s string) error {
	if s == "" {
		if *l != nil {
			delete(*l, n)
		}
		return nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return err
	}
	if *l == nil {
		*l = corev1.ResourceList{}
	}
	(*l)[n] = q
	return nil
}

func restore(c *corev1.Container, r ContainerResources) error {
	for _, s := range []struct {
		l *corev1.ResourceList
		n corev1.ResourceName
		v string
	}{
		{&c.Resources.Requests, corev1.ResourceCPU, r.CPURequest}, {&c.Resources.Limits, corev1.ResourceCPU, r.CPULimit},
		{&c.Resources.Requests, corev1.ResourceMemory, r.MemRequest}, {&c.Resources.Limits, corev1.ResourceMemory, r.MemLimit},
	} {
		if err := setQty(s.l, s.n, s.v); err != nil {
			return err
		}
	}
	return nil
}

// Inspect gathers the facts the policy needs. Any API failure returns Known=false (the policy then denies).
func (e *Executor) Inspect(ctx context.Context, ns, name string) (safety.Facts, *appsv1.Deployment, error) {
	f := safety.Facts{Namespace: ns, Kind: "Deployment", Name: name}
	d, err := e.KC.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return f, nil, fmt.Errorf("get deployment: %w", err)
	}
	f.Labels = d.Labels
	f.Approved = d.Annotations[AnnApproved]
	f.AlreadyApplied = d.Annotations[AnnApplied]
	f.RolledBack = d.Annotations[AnnRolledBack]
	f.GitOps = gitops(d)

	hpas, err := e.KC.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return f, d, fmt.Errorf("list hpas: %w", err)
	}
	for _, h := range hpas.Items {
		if h.Spec.ScaleTargetRef.Kind != "Deployment" || h.Spec.ScaleTargetRef.Name != name {
			continue
		}
		for _, m := range h.Spec.Metrics {
			if m.Type == "Resource" && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU {
				f.HPAOnCPU = true
			}
		}
	}

	if e.Dyn == nil {
		return f, d, errors.New("no dynamic client: cannot rule out a VPA")
	}
	vpas, err := e.Dyn.Resource(vpaGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		for _, v := range vpas.Items {
			tn, _, _ := unstructuredString(v.Object, "spec", "targetRef", "name")
			if tn != name {
				continue
			}
			mode, found, _ := unstructuredString(v.Object, "spec", "updatePolicy", "updateMode")
			if !found || mode == "Auto" || mode == "Recreate" || mode == "Initial" {
				f.VPAAuto = true
			}
		}
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// the VPA CRD is not installed: there is no VPA
	default:
		return f, d, fmt.Errorf("list vpas: %w", err)
	}
	f.Known = true
	return f, d, nil
}

func unstructuredString(o map[string]any, path ...string) (string, bool, error) {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		if cur, ok = m[p]; !ok {
			return "", false, nil
		}
	}
	s, ok := cur.(string)
	return s, ok, nil
}

func gitops(d *appsv1.Deployment) string {
	has := func(k string) bool {
		_, a := d.Annotations[k]
		_, l := d.Labels[k]
		return a || l
	}
	switch {
	case has("argocd.argoproj.io/tracking-id") || has("argocd.argoproj.io/instance"):
		return "argocd"
	case has("kustomize.toolkit.fluxcd.io/name") || has("helm.toolkit.fluxcd.io/name"):
		return "flux"
	}
	return ""
}

// recentApplies returns the timestamps of changes this tool made in the namespace (for the rate limit).
func (e *Executor) recentApplies(ctx context.Context, ns string) ([]time.Time, error) {
	ds, err := e.KC.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []time.Time
	for _, d := range ds.Items {
		if s, ok := d.Annotations[AnnAppliedAt]; ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// Result describes what Apply did.
type Result struct {
	Decision safety.Decision
	Facts    safety.Facts
	DryRun   bool
	Applied  bool
	Previous *Previous
}

// Apply evaluates the policy and, only if allowed AND execute is true, changes the Deployment.
// It is idempotent: the same recommendation ID is never applied twice, and a lost update race is an error, not an overwrite.
func (e *Executor) Apply(ctx context.Context, rec recommend.Recommendation, pol safety.Policy, execute bool) (Result, error) {
	var res Result
	if rec.Kind != "Deployment" {
		return res, fmt.Errorf("kind %q is not supported (Deployment only)", rec.Kind)
	}
	facts, dep, err := e.Inspect(ctx, rec.Namespace, rec.Workload)
	res.Facts = facts
	if err != nil {
		facts.Known = false
		res.Facts = facts
		e.record("inspect_failed", rec, map[string]any{"error": err.Error()})
		if dep == nil {
			return res, err
		}
	}
	recent, rerr := e.recentApplies(ctx, rec.Namespace)
	if rerr != nil {
		facts.Known = false
		res.Facts = facts
	}
	res.Decision = safety.Evaluate(pol, rec, facts, e.now(), recent)
	if dep != nil {
		// a recommendation made for other values than the live ones is stale
		if c := findContainer(dep, rec.Container); c == nil {
			res.Decision.Allowed = false
			res.Decision.Reasons = append(res.Decision.Reasons, fmt.Sprintf("container %q not found", rec.Container))
		} else if why := stale(*c, rec); why != "" {
			res.Decision.Allowed = false
			res.Decision.Reasons = append(res.Decision.Reasons, why)
		} else if why := limitLowered(*c, rec); why != "" {
			res.Decision.Allowed = false
			res.Decision.Reasons = append(res.Decision.Reasons, why)
		}
	}
	e.record("decision", rec, map[string]any{"allowed": res.Decision.Allowed, "reasons": res.Decision.Reasons, "execute": execute})
	if !res.Decision.Allowed {
		return res, nil
	}
	if !execute {
		res.DryRun = true
		e.record("dry_run", rec, map[string]any{"cpu": rec.CPU.Recommended, "memory": rec.Memory.Recommended})
		return res, nil
	}

	// Write phase. Update carries the resourceVersion, so a concurrent change is a conflict, never an overwrite. The deployment controller
	// also bumps the resourceVersion when it updates STATUS, which is harmless: we retry with a fresh read as long as the SPEC generation is
	// the one we inspected, and abort the moment someone else changed the spec.
	gen := dep.Generation
	var prev Previous
	for attempt := 0; ; attempt++ {
		cur, err := e.KC.AppsV1().Deployments(rec.Namespace).Get(ctx, rec.Workload, metav1.GetOptions{})
		if err != nil {
			return res, err
		}
		if cur.Generation != gen {
			e.record("apply_failed", rec, map[string]any{"error": "spec changed concurrently"})
			return res, fmt.Errorf("the Deployment spec changed while the change was being prepared (generation %d -> %d); nothing was changed", gen, cur.Generation)
		}
		prev = Previous{RecommendationID: rec.ID, Actor: e.Actor, At: e.now().UTC(), Containers: map[string]ContainerResources{}}
		if cur.Spec.Replicas != nil {
			prev.Replicas = *cur.Spec.Replicas
		}
		for _, c := range cur.Spec.Template.Spec.Containers {
			prev.Containers[c.Name] = capture(c)
		}
		c := findContainer(cur, rec.Container)
		if c == nil {
			return res, fmt.Errorf("container %q disappeared", rec.Container)
		}
		if why := stale(*c, rec); why != "" {
			return res, fmt.Errorf("%s (nothing was changed)", why)
		}
		if err := applyRec(c, rec); err != nil {
			return res, err
		}
		raw, _ := json.Marshal(prev)
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[AnnPrevious] = string(raw)
		cur.Annotations[AnnApplied] = rec.ID
		cur.Annotations[AnnAppliedAt] = prev.At.Format(time.RFC3339)
		delete(cur.Annotations, AnnRolledBack)
		_, err = e.KC.AppsV1().Deployments(rec.Namespace).Update(ctx, cur, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if apierrors.IsConflict(err) && attempt < 4 {
			continue
		}
		e.record("apply_failed", rec, map[string]any{"error": err.Error()})
		return res, fmt.Errorf("update deployment (nothing was changed): %w", err)
	}
	res.Applied, res.Previous = true, &prev
	e.record("applied", rec, map[string]any{"previous": prev.Containers, "cpu": rec.CPU.Recommended, "memory": rec.Memory.Recommended})
	return res, nil
}

func findContainer(d *appsv1.Deployment, name string) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == name {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// stale reports why the live values differ from the ones the recommendation was computed against.
func stale(c corev1.Container, rec recommend.Recommendation) string {
	chk := func(name string, live *resource.Quantity, want float64) string {
		got := 0.0
		if live != nil {
			got = live.AsApproximateFloat64()
		}
		if math.Abs(got-want) > 0.01*math.Max(want, 1e-9) && !(got == 0 && want == 0) {
			return fmt.Sprintf("stale recommendation: live %s request is %.6g but it was computed for %.6g", name, got, want)
		}
		return ""
	}
	var cpu, mem *resource.Quantity
	if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		cpu = &v
	}
	if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		mem = &v
	}
	if s := chk("cpu", cpu, rec.CPU.Current); s != "" {
		return s
	}
	return chk("memory", mem, rec.Memory.Current)
}

// limitLowered refuses a recommendation that would lower an existing limit or set one below the new request (v1 never lowers limits).
func limitLowered(c corev1.Container, rec recommend.Recommendation) string {
	for _, r := range []struct {
		name string
		res  corev1.ResourceName
		rr   recommend.ResourceResult
	}{{"cpu", corev1.ResourceCPU, rec.CPU}, {"memory", corev1.ResourceMemory, rec.Memory}} {
		if r.rr.Action != recommend.Increase && r.rr.Action != recommend.Decrease {
			continue
		}
		live, has := c.Resources.Limits[r.res]
		switch {
		case has && r.rr.Limit > 0 && r.rr.Limit < live.AsApproximateFloat64()*0.999:
			return fmt.Sprintf("refused: the recommendation would lower the %s limit from %s", r.name, live.String())
		case !has && r.rr.Limit > 0:
			return fmt.Sprintf("refused: the recommendation would set a new %s limit where none exists", r.name)
		case r.rr.Limit > 0 && r.rr.Limit < r.rr.Recommended:
			return fmt.Sprintf("refused: the %s limit is below the recommended request", r.name)
		}
	}
	return ""
}

func applyRec(c *corev1.Container, rec recommend.Recommendation) error {
	if rec.CPU.Action == recommend.Increase || rec.CPU.Action == recommend.Decrease {
		q := resource.NewMilliQuantity(int64(math.Round(rec.CPU.Recommended*1000)), resource.DecimalSI)
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[corev1.ResourceCPU] = *q
		if rec.CPU.Limit > 0 {
			c.Resources.Limits = ensure(c.Resources.Limits)
			c.Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(math.Round(rec.CPU.Limit*1000)), resource.DecimalSI)
		}
	}
	if rec.Memory.Action == recommend.Increase || rec.Memory.Action == recommend.Decrease {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(int64(math.Round(rec.Memory.Recommended)), resource.BinarySI)
		if rec.Memory.Limit > 0 {
			c.Resources.Limits = ensure(c.Resources.Limits)
			c.Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(int64(math.Round(rec.Memory.Limit)), resource.BinarySI)
		}
	}
	return nil
}

func ensure(l corev1.ResourceList) corev1.ResourceList {
	if l == nil {
		return corev1.ResourceList{}
	}
	return l
}

// ---------------------------------------------------------------- observation and rollback

// Thresholds define "degraded". Zero values take the defaults shown.
type Thresholds struct {
	RolloutTimeout time.Duration // 5m: the new template must be fully rolled out and available by then
	ObserveFor     time.Duration // 5m: how long the stable state must hold
	Poll           time.Duration // 10s
	MaxRestarts    int           // 0: any container restart of the new pods counts
	// SLO, if set, is polled each round. breached=true degrades; known=false is ignored (but reported).
	SLO func(ctx context.Context) (breached, known bool, err error)
}

func (t Thresholds) def() Thresholds {
	if t.RolloutTimeout == 0 {
		t.RolloutTimeout = 5 * time.Minute
	}
	if t.ObserveFor == 0 {
		t.ObserveFor = 5 * time.Minute
	}
	if t.Poll == 0 {
		t.Poll = 10 * time.Second
	}
	return t
}

// Observation is one reading of the workload after a change.
type Observation struct {
	RolloutComplete  bool
	Replicas         int32 // desired
	AvailableReplica int32
	Restarts         int
	OOMKilled        int
	NotReady         int
	SLOBreached      bool
}

// Judge is the pure rollback rule: it returns every reason the observation is degraded (empty = healthy).
// baselineReplicas guards against the change itself or an HPA moving the replica count unexpectedly.
func Judge(o Observation, baselineReplicas int32, th Thresholds) []string {
	var r []string
	if o.OOMKilled > 0 {
		r = append(r, fmt.Sprintf("%d container(s) OOMKilled", o.OOMKilled))
	}
	if o.Restarts > th.MaxRestarts {
		r = append(r, fmt.Sprintf("%d container restart(s) (max %d)", o.Restarts, th.MaxRestarts))
	}
	if o.RolloutComplete && o.AvailableReplica < o.Replicas {
		r = append(r, fmt.Sprintf("only %d of %d replicas available", o.AvailableReplica, o.Replicas))
	}
	if o.RolloutComplete && o.NotReady > 0 {
		r = append(r, fmt.Sprintf("%d pod(s) not ready (readiness failing)", o.NotReady))
	}
	if baselineReplicas > 0 && o.Replicas != baselineReplicas {
		r = append(r, fmt.Sprintf("replica count changed from %d to %d during observation", baselineReplicas, o.Replicas))
	}
	if o.SLOBreached {
		r = append(r, "SLO breached")
	}
	return r
}

func (e *Executor) observe(ctx context.Context, ns, name string) (Observation, error) {
	var o Observation
	d, err := e.KC.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return o, err
	}
	if d.Spec.Replicas != nil {
		o.Replicas = *d.Spec.Replicas
	}
	o.AvailableReplica = d.Status.AvailableReplicas
	o.RolloutComplete = d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == o.Replicas && d.Status.Replicas == o.Replicas && d.Status.AvailableReplicas == o.Replicas
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return o, err
	}
	pods, err := e.KC.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return o, err
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			o.NotReady++
		}
		for _, cs := range p.Status.ContainerStatuses {
			o.Restarts += int(cs.RestartCount)
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				o.OOMKilled++
			}
			if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
				o.OOMKilled++
			}
		}
	}
	return o, nil
}

// Verdict is the result of Watch.
type Verdict struct {
	Healthy bool
	Reasons []string
}

// Watch waits for the rollout, then observes for th.ObserveFor. It returns early as soon as the workload is degraded.
// Pods created by the rollout start with fresh restart counters, so any restart is attributable to the change.
func (e *Executor) Watch(ctx context.Context, rec recommend.Recommendation, th Thresholds) (Verdict, error) {
	th = th.def()
	start := e.now()
	var baseline int32
	var stableSince time.Time
	for {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		o, err := e.observe(ctx, rec.Namespace, rec.Workload)
		if err != nil {
			return Verdict{}, err
		}
		if th.SLO != nil {
			if b, known, err := th.SLO(ctx); err != nil {
				return Verdict{}, fmt.Errorf("slo check: %w", err)
			} else if known {
				o.SLOBreached = b
			}
		}
		if o.RolloutComplete && stableSince.IsZero() {
			stableSince, baseline = e.now(), o.Replicas
		}
		if reasons := Judge(o, baseline, th); len(reasons) > 0 && (o.RolloutComplete || o.OOMKilled > 0 || o.SLOBreached) {
			e.record("degraded", rec, map[string]any{"reasons": reasons})
			return Verdict{Reasons: reasons}, nil
		}
		switch {
		case !o.RolloutComplete && e.now().Sub(start) >= th.RolloutTimeout:
			rs := []string{fmt.Sprintf("rollout did not complete within %s", th.RolloutTimeout)}
			e.record("degraded", rec, map[string]any{"reasons": rs})
			return Verdict{Reasons: rs}, nil
		case o.RolloutComplete && e.now().Sub(stableSince) >= th.ObserveFor:
			e.record("verified", rec, map[string]any{"observedFor": th.ObserveFor.String()})
			return Verdict{Healthy: true}, nil
		}
		e.sleep(th.Poll)
	}
}

// Rollback restores the saved state, waits for the rollout and verifies the restored values by reading them back.
func (e *Executor) Rollback(ctx context.Context, ns, name string, reasons []string, wait time.Duration) error {
	rec := recommend.Recommendation{Namespace: ns, Workload: name, Kind: "Deployment"}
	var prev Previous
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		d, err := e.KC.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		raw, ok := d.Annotations[AnnPrevious]
		if !ok {
			return errNoSavedState
		}
		if err := json.Unmarshal([]byte(raw), &prev); err != nil {
			return fmt.Errorf("saved previous state is unreadable: %w", err)
		}
		for i := range d.Spec.Template.Spec.Containers {
			c := &d.Spec.Template.Spec.Containers[i]
			if p, ok := prev.Containers[c.Name]; ok {
				if err := restore(c, p); err != nil {
					return err
				}
			}
		}
		delete(d.Annotations, AnnPrevious)
		delete(d.Annotations, AnnApplied)
		d.Annotations[AnnRolledBack] = prev.RecommendationID
		_, err = e.KC.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		if errors.Is(err, errNoSavedState) {
			return errors.New("no saved previous state on this workload: nothing to roll back")
		}
		e.record("rollback_failed", rec, map[string]any{"error": err.Error()})
		return fmt.Errorf("rollback update: %w", err)
	}
	rec.ID = prev.RecommendationID
	// verify what was written
	got, err := e.KC.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var bad []string
	for _, c := range got.Spec.Template.Spec.Containers {
		if p, ok := prev.Containers[c.Name]; ok && !sameResources(capture(c), p) {
			bad = append(bad, c.Name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		e.record("rollback_failed", rec, map[string]any{"mismatch": bad})
		return fmt.Errorf("rollback verification failed for %s", strings.Join(bad, ","))
	}
	e.record("rolled_back", rec, map[string]any{"reasons": reasons, "restored": prev.Containers, "verified": true})
	if wait > 0 {
		deadline := e.now().Add(wait)
		for e.now().Before(deadline) {
			o, err := e.observe(ctx, ns, name)
			if err != nil {
				return err
			}
			if o.RolloutComplete {
				return nil
			}
			e.sleep(5 * time.Second)
		}
		return fmt.Errorf("restored values are in place but the rollout did not complete within %s", wait)
	}
	return nil
}

func sameResources(a, b ContainerResources) bool {
	eq := func(x, y string) bool {
		if x == "" || y == "" {
			return x == y
		}
		qx, e1 := resource.ParseQuantity(x)
		qy, e2 := resource.ParseQuantity(y)
		return e1 == nil && e2 == nil && qx.Cmp(qy) == 0
	}
	return eq(a.CPURequest, b.CPURequest) && eq(a.CPULimit, b.CPULimit) && eq(a.MemRequest, b.MemRequest) && eq(a.MemLimit, b.MemLimit)
}

// ApplyWatchRollback is the full controlled change: apply, watch, and roll back on degradation.
// A rollback that itself fails is returned as an error so a human is alerted.
func (e *Executor) ApplyWatchRollback(ctx context.Context, rec recommend.Recommendation, pol safety.Policy, th Thresholds) (Result, Verdict, error) {
	res, err := e.Apply(ctx, rec, pol, true)
	if err != nil || !res.Applied {
		return res, Verdict{}, err
	}
	v, err := e.Watch(ctx, rec, th)
	if err != nil {
		// we cannot tell whether it is healthy: fail safe and restore
		if rerr := e.Rollback(ctx, rec.Namespace, rec.Workload, []string{"watch failed: " + err.Error()}, th.def().RolloutTimeout); rerr != nil {
			return res, v, fmt.Errorf("watch failed (%v) and rollback failed: %w", err, rerr)
		}
		return res, v, fmt.Errorf("watch failed, change rolled back: %w", err)
	}
	if !v.Healthy {
		if rerr := e.Rollback(ctx, rec.Namespace, rec.Workload, v.Reasons, th.def().RolloutTimeout); rerr != nil {
			return res, v, rerr
		}
	}
	return res, v, nil
}
