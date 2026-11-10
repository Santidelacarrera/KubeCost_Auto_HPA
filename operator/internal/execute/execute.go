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
	authv1 "k8s.io/api/authentication/v1"
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
	AnnApprovedBy = "autoscaling.kubecost.io/approved-by"
	AnnApprovedAt = "autoscaling.kubecost.io/approved-at"
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
	// ApprovalOverride, if set, replaces the approval annotations on the workload as the source of the approval: it returns the approved
	// recommendation ID and the approver identity, or ok=false for "not approved". The applier controller uses it so approval comes from
	// a WorkloadRecommendation status (its own RBAC permission) and annotations on the workload are ignored.
	ApprovalOverride func(kind, ns, name string) (id, by string, ok bool)
	Now              func() time.Time
	Sleep            func(time.Duration)
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

// Supported workload kinds.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// workload hides the differences between the three supported kinds: the pod template to change, how to tell a rollout is complete,
// and how to write the object back.
type workload struct {
	kind        string
	obj         metav1.Object
	tmpl        *corev1.PodTemplateSpec
	sel         *metav1.LabelSelector
	desired     int32 // replicas the controller wants (DaemonSet: nodes it must run on)
	replicas    int32 // spec replicas; 0 for a DaemonSet
	rolloutDone bool
	available   int32
	onDelete    bool // pods are NOT replaced by a template change: the change would never roll out
	save        func(ctx context.Context) error
}

func (e *Executor) get(ctx context.Context, kind, ns, name string) (*workload, error) {
	switch kind {
	case KindDeployment:
		d, err := e.KC.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &workload{kind: kind, obj: d, tmpl: &d.Spec.Template, sel: d.Spec.Selector, available: d.Status.AvailableReplicas}
		if d.Spec.Replicas != nil {
			w.replicas = *d.Spec.Replicas
		}
		w.desired = w.replicas
		w.rolloutDone = d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == w.desired && d.Status.Replicas == w.desired && d.Status.AvailableReplicas == w.desired
		w.save = func(ctx context.Context) error {
			_, err := e.KC.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})
			return err
		}
		return w, nil
	case KindStatefulSet:
		d, err := e.KC.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &workload{kind: kind, obj: d, tmpl: &d.Spec.Template, sel: d.Spec.Selector, available: d.Status.AvailableReplicas,
			onDelete: d.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType}
		if d.Spec.Replicas != nil {
			w.replicas = *d.Spec.Replicas
		}
		w.desired = w.replicas
		w.rolloutDone = d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == w.desired && d.Status.ReadyReplicas == w.desired && d.Status.AvailableReplicas == w.desired
		w.save = func(ctx context.Context) error {
			_, err := e.KC.AppsV1().StatefulSets(ns).Update(ctx, d, metav1.UpdateOptions{})
			return err
		}
		return w, nil
	case KindDaemonSet:
		d, err := e.KC.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &workload{kind: kind, obj: d, tmpl: &d.Spec.Template, sel: d.Spec.Selector, available: d.Status.NumberAvailable,
			desired: d.Status.DesiredNumberScheduled, onDelete: d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType}
		w.rolloutDone = d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedNumberScheduled == w.desired && d.Status.NumberAvailable == w.desired
		w.save = func(ctx context.Context) error {
			_, err := e.KC.AppsV1().DaemonSets(ns).Update(ctx, d, metav1.UpdateOptions{})
			return err
		}
		return w, nil
	}
	return nil, fmt.Errorf("kind %q is not supported (Deployment, StatefulSet, DaemonSet)", kind)
}

// Inspect gathers the facts the policy needs. Any API failure returns Known=false (the policy then denies).
func (e *Executor) Inspect(ctx context.Context, kind, ns, name string) (safety.Facts, *workload, error) {
	f := safety.Facts{Namespace: ns, Kind: kind, Name: name}
	w, err := e.get(ctx, kind, ns, name)
	if err != nil {
		return f, nil, fmt.Errorf("get %s: %w", strings.ToLower(kind), err)
	}
	ann, lbl := w.obj.GetAnnotations(), w.obj.GetLabels()
	f.Labels = lbl
	f.Approved = ann[AnnApproved]
	f.ApprovedBy = ann[AnnApprovedBy]
	if e.ApprovalOverride != nil {
		f.Approved, f.ApprovedBy = "", ""
		if id, by, ok := e.ApprovalOverride(kind, ns, name); ok {
			f.Approved, f.ApprovedBy = id, by
		}
	}
	f.AlreadyApplied = ann[AnnApplied]
	f.RolledBack = ann[AnnRolledBack]
	f.GitOps = gitops(w.obj)

	hpas, err := e.KC.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return f, w, fmt.Errorf("list hpas: %w", err)
	}
	for _, h := range hpas.Items {
		if h.Spec.ScaleTargetRef.Kind != kind || h.Spec.ScaleTargetRef.Name != name {
			continue
		}
		for _, m := range h.Spec.Metrics {
			if m.Type == "Resource" && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU {
				f.HPAOnCPU = true
			}
		}
	}

	if e.Dyn == nil {
		return f, w, errors.New("no dynamic client: cannot rule out a VPA")
	}
	vpas, err := e.Dyn.Resource(vpaGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		for _, v := range vpas.Items {
			tn, _, _ := unstructuredString(v.Object, "spec", "targetRef", "name")
			tk, _, _ := unstructuredString(v.Object, "spec", "targetRef", "kind")
			if tn != name || (tk != "" && tk != kind) {
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
		return f, w, fmt.Errorf("list vpas: %w", err)
	}
	f.Known = true
	return f, w, nil
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

func gitops(o metav1.Object) string {
	has := func(k string) bool {
		_, a := o.GetAnnotations()[k]
		_, l := o.GetLabels()[k]
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
	var anns []map[string]string
	ds, err := e.KC.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range ds.Items {
		anns = append(anns, d.Annotations)
	}
	ss, err := e.KC.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range ss.Items {
		anns = append(anns, d.Annotations)
	}
	dss, err := e.KC.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range dss.Items {
		anns = append(anns, d.Annotations)
	}
	var out []time.Time
	for _, a := range anns {
		if s, ok := a[AnnAppliedAt]; ok {
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

// Apply evaluates the policy and, only if allowed AND execute is true, changes the workload.
// It is idempotent: the same recommendation ID is never applied twice, and a lost update race is an error, not an overwrite.
func (e *Executor) Apply(ctx context.Context, rec recommend.Recommendation, pol safety.Policy, execute bool) (Result, error) {
	var res Result
	switch rec.Kind {
	case KindDeployment, KindStatefulSet, KindDaemonSet:
	default:
		return res, fmt.Errorf("kind %q is not supported (Deployment, StatefulSet, DaemonSet)", rec.Kind)
	}
	facts, w, err := e.Inspect(ctx, rec.Kind, rec.Namespace, rec.Workload)
	res.Facts = facts
	if err != nil {
		facts.Known = false
		res.Facts = facts
		e.record("inspect_failed", rec, map[string]any{"error": err.Error()})
		if w == nil {
			return res, err
		}
	}
	recent, rerr := e.recentApplies(ctx, rec.Namespace)
	if rerr != nil {
		facts.Known = false
		res.Facts = facts
	}
	res.Decision = safety.Evaluate(pol, rec, facts, e.now(), recent)
	if w != nil {
		deny := func(why string) {
			res.Decision.Allowed = false
			res.Decision.Reasons = append(res.Decision.Reasons, why)
		}
		if w.onDelete {
			deny("the update strategy is OnDelete: a template change would not replace the pods, so the change would never roll out")
		}
		// a recommendation made for other values than the live ones is stale
		if c := findContainer(w.tmpl, rec.Container); c == nil {
			deny(fmt.Sprintf("container %q not found", rec.Container))
		} else if why := stale(*c, rec); why != "" {
			deny(why)
		} else if why := limitLowered(*c, rec); why != "" {
			deny(why)
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

	// Write phase. Update carries the resourceVersion, so a concurrent change is a conflict, never an overwrite. The controllers also bump
	// the resourceVersion when they update STATUS, which is harmless: we retry with a fresh read as long as the SPEC generation is
	// the one we inspected, and abort the moment someone else changed the spec.
	gen := w.obj.GetGeneration()
	var prev Previous
	for attempt := 0; ; attempt++ {
		cur, err := e.get(ctx, rec.Kind, rec.Namespace, rec.Workload)
		if err != nil {
			return res, err
		}
		if cur.obj.GetGeneration() != gen {
			e.record("apply_failed", rec, map[string]any{"error": "spec changed concurrently"})
			return res, fmt.Errorf("the %s spec changed while the change was being prepared (generation %d -> %d); nothing was changed", rec.Kind, gen, cur.obj.GetGeneration())
		}
		prev = Previous{RecommendationID: rec.ID, Actor: e.Actor, At: e.now().UTC(), Containers: map[string]ContainerResources{}, Replicas: cur.replicas}
		for _, c := range cur.tmpl.Spec.Containers {
			prev.Containers[c.Name] = capture(c)
		}
		c := findContainer(cur.tmpl, rec.Container)
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
		ann := cur.obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnPrevious] = string(raw)
		ann[AnnApplied] = rec.ID
		ann[AnnAppliedAt] = prev.At.Format(time.RFC3339)
		delete(ann, AnnRolledBack)
		cur.obj.SetAnnotations(ann)
		err = cur.save(ctx)
		if err == nil {
			break
		}
		if apierrors.IsConflict(err) && attempt < 4 {
			continue
		}
		e.record("apply_failed", rec, map[string]any{"error": err.Error()})
		return res, fmt.Errorf("update %s (nothing was changed): %w", strings.ToLower(rec.Kind), err)
	}
	res.Applied, res.Previous = true, &prev
	e.record("applied", rec, map[string]any{"previous": prev.Containers, "cpu": rec.CPU.Recommended, "memory": rec.Memory.Recommended,
		"cpuPotentialMonthlyUSD": rec.CPU.PotentialMonthlyUSD, "memoryPotentialMonthlyUSD": rec.Memory.PotentialMonthlyUSD, "approvedBy": facts.ApprovedBy})
	return res, nil
}

func findContainer(t *corev1.PodTemplateSpec, name string) *corev1.Container {
	for i := range t.Spec.Containers {
		if t.Spec.Containers[i].Name == name {
			return &t.Spec.Containers[i]
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
	// ObserveErrorTolerance (2m): how long reads of the workload (or the SLO query) may keep failing before Watch gives up and the change is
	// rolled back as a precaution. Not being able to look is not evidence of a problem, so short outages of the API server or of Prometheus are
	// ridden out; but the unobserved time never counts as healthy time: the observation window restarts once reads work again.
	ObserveErrorTolerance time.Duration
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
	if t.ObserveErrorTolerance == 0 {
		t.ObserveErrorTolerance = 2 * time.Minute
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

func (e *Executor) observe(ctx context.Context, kind, ns, name string) (Observation, error) {
	var o Observation
	w, err := e.get(ctx, kind, ns, name)
	if err != nil {
		return o, err
	}
	o.Replicas, o.AvailableReplica, o.RolloutComplete = w.desired, w.available, w.rolloutDone
	sel, err := metav1.LabelSelectorAsSelector(w.sel)
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
	var stableSince, failingSince time.Time
	// blind handles a failed read: ride it out for a bounded time, never counting the unobserved time as healthy.
	blind := func(err error) error {
		if failingSince.IsZero() {
			failingSince = e.now()
			e.record("observe_error", rec, map[string]any{"error": err.Error(), "tolerating": th.ObserveErrorTolerance.String()})
		}
		stableSince = time.Time{}
		if e.now().Sub(failingSince) >= th.ObserveErrorTolerance {
			return err
		}
		e.sleep(th.Poll)
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		o, err := e.observe(ctx, rec.Kind, rec.Namespace, rec.Workload)
		if err != nil {
			if berr := blind(err); berr != nil {
				return Verdict{}, berr
			}
			continue
		}
		if th.SLO != nil {
			if b, known, err := th.SLO(ctx); err != nil {
				if berr := blind(fmt.Errorf("slo check: %w", err)); berr != nil {
					return Verdict{}, berr
				}
				continue
			} else if known {
				o.SLOBreached = b
			}
		}
		if !failingSince.IsZero() {
			// While the API (or the SLO source) was unreachable the rollout could not be judged and the control plane may not have
			// been able to progress it either: that time must not count against the rollout timeout (found in a real API-server outage).
			start = start.Add(e.now().Sub(failingSince))
			failingSince = time.Time{}
		}
		if o.RolloutComplete && stableSince.IsZero() {
			stableSince, baseline = e.now(), o.Replicas
			if rec.Kind == KindDaemonSet {
				baseline = 0 // the desired count follows the node count, which says nothing about this change
			}
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
func (e *Executor) Rollback(ctx context.Context, kind, ns, name string, reasons []string, wait time.Duration) error {
	rec := recommend.Recommendation{Namespace: ns, Workload: name, Kind: kind}
	var prev Previous
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		w, err := e.get(ctx, kind, ns, name)
		if err != nil {
			return err
		}
		ann := w.obj.GetAnnotations()
		raw, ok := ann[AnnPrevious]
		if !ok {
			return errNoSavedState
		}
		if err := json.Unmarshal([]byte(raw), &prev); err != nil {
			return fmt.Errorf("saved previous state is unreadable: %w", err)
		}
		for i := range w.tmpl.Spec.Containers {
			c := &w.tmpl.Spec.Containers[i]
			if p, ok := prev.Containers[c.Name]; ok {
				if err := restore(c, p); err != nil {
					return err
				}
			}
		}
		delete(ann, AnnPrevious)
		delete(ann, AnnApplied)
		ann[AnnRolledBack] = prev.RecommendationID
		w.obj.SetAnnotations(ann)
		return w.save(ctx)
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
	got, err := e.get(ctx, kind, ns, name)
	if err != nil {
		return err
	}
	var bad []string
	for _, c := range got.tmpl.Spec.Containers {
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
			o, err := e.observe(ctx, kind, ns, name)
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
	v, err := e.WatchAndRollback(ctx, rec, th)
	return res, v, err
}

// WatchAndRollback observes a change that is already applied and restores the previous state if it degrades. It is also how an
// interrupted run is resumed: the state it needs lives on the workload. If the context is cancelled (the process is shutting down)
// it returns the context error WITHOUT rolling back, because "we stopped looking" is not evidence that the workload is unhealthy.
func (e *Executor) WatchAndRollback(ctx context.Context, rec recommend.Recommendation, th Thresholds) (Verdict, error) {
	v, err := e.Watch(ctx, rec, th)
	if err != nil {
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		// we cannot tell whether it is healthy: fail safe and restore
		if rerr := e.Rollback(ctx, rec.Kind, rec.Namespace, rec.Workload, []string{"watch failed: " + err.Error()}, th.def().RolloutTimeout); rerr != nil {
			return v, fmt.Errorf("watch failed (%v) and rollback failed: %w", err, rerr)
		}
		return v, fmt.Errorf("watch failed, change rolled back: %w", err)
	}
	if !v.Healthy {
		if rerr := e.Rollback(ctx, rec.Kind, rec.Namespace, rec.Workload, v.Reasons, th.def().RolloutTimeout); rerr != nil {
			return v, rerr
		}
	}
	return v, nil
}

// ---------------------------------------------------------------- approval with identity

// Identity asks the API server who the current credentials belong to (SelfSubjectReview). The answer is the server's, not the caller's.
func (e *Executor) Identity(ctx context.Context) (string, error) {
	r, err := e.KC.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("cannot establish who you are (SelfSubjectReview): %w", err)
	}
	if r.Status.UserInfo.Username == "" {
		return "", errors.New("the API server returned no username for these credentials")
	}
	return r.Status.UserInfo.Username, nil
}

// Approve records the approval of exactly this recommendation, stamped with the identity the API server reports.
// It refuses abstentions and recommendations that change nothing, and a recommendation that does not match the live object.
func (e *Executor) Approve(ctx context.Context, rec recommend.Recommendation) (string, error) {
	if rec.Abstained || !(rec.CPU.Action == recommend.Increase || rec.CPU.Action == recommend.Decrease ||
		rec.Memory.Action == recommend.Increase || rec.Memory.Action == recommend.Decrease) {
		return "", errors.New("this recommendation changes nothing (or abstained): there is nothing to approve")
	}
	who, err := e.Identity(ctx)
	if err != nil {
		return "", err
	}
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		w, err := e.get(ctx, rec.Kind, rec.Namespace, rec.Workload)
		if err != nil {
			return err
		}
		c := findContainer(w.tmpl, rec.Container)
		if c == nil {
			return fmt.Errorf("container %q not found", rec.Container)
		}
		if why := stale(*c, rec); why != "" {
			return errors.New(why)
		}
		ann := w.obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnApproved] = rec.ID
		ann[AnnApprovedBy] = who
		ann[AnnApprovedAt] = e.now().UTC().Format(time.RFC3339)
		w.obj.SetAnnotations(ann)
		return w.save(ctx)
	})
	if err != nil {
		return "", err
	}
	e.record("approved", rec, map[string]any{"approvedBy": who})
	return who, nil
}

// Fresh reports whether the live workload still has the values the recommendation was computed for (and the container exists).
// It reads only.
func (e *Executor) Fresh(ctx context.Context, rec recommend.Recommendation) error {
	w, err := e.get(ctx, rec.Kind, rec.Namespace, rec.Workload)
	if err != nil {
		return err
	}
	c := findContainer(w.tmpl, rec.Container)
	if c == nil {
		return fmt.Errorf("container %q not found", rec.Container)
	}
	if why := stale(*c, rec); why != "" {
		return errors.New(why)
	}
	return nil
}
